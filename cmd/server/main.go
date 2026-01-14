package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"porteight-camera-ai/internal/db"
	"porteight-camera-ai/internal/stream"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func main() {
	// Storage paths
	// - If PERSIST_DIR is set (e.g. /workspace on RunPod), we default all writable paths under it.
	// - You can override individually with DB_PATH, RECORDINGS_DIR, HLS_DIR.
	persistDirEnv := os.Getenv("PERSIST_DIR")
	defaultDBPath := "camera_ai.db"
	defaultRecDir := "./recordings"
	defaultHLSDir := "./web/static/hls"
	if persistDirEnv != "" {
		defaultDBPath = filepath.Join(persistDirEnv, "camera_ai.db")
		defaultRecDir = filepath.Join(persistDirEnv, "recordings")
		defaultHLSDir = filepath.Join(persistDirEnv, "hls")
	}

	dbPathEnv := os.Getenv("DB_PATH")
	if dbPathEnv == "" {
		dbPathEnv = defaultDBPath
	}
	recDirEnv := os.Getenv("RECORDINGS_DIR")
	if recDirEnv == "" {
		recDirEnv = defaultRecDir
	}
	hlsDirEnv := os.Getenv("HLS_DIR")
	if hlsDirEnv == "" {
		hlsDirEnv = defaultHLSDir
	}

	// Optional CLI overrides (handy for local dev / containers)
	dbPath := flag.String("db", dbPathEnv, "path to sqlite db file")
	recDir := flag.String("recordings", recDirEnv, "directory to store recordings")
	hlsDir := flag.String("hls", hlsDirEnv, "directory to store HLS output (m3u8/ts/json)")
	flag.Parse()

	// Ensure DB directory exists (sqlite won't create parent dirs)
	if dir := filepath.Dir(*dbPath); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("Failed to create db directory %s: %v", dir, err)
		}
	}

	// Initialize DB
	if err := db.Init(*dbPath); err != nil {
		log.Fatalf("Failed to init db: %v", err)
	}

	// Start RTMP Server
	rtmpSrv := stream.NewServer(":1935", *hlsDir, *recDir)
	go func() {
		if err := rtmpSrv.Start(); err != nil {
			log.Fatalf("RTMP Server failed: %v", err)
		}
	}()

	// Initialize Web Server
	r := gin.Default()

	// Session Store
	store := cookie.NewStore([]byte("secret")) // Change "secret" to env var in production
	r.Use(sessions.Sessions("mysession", store))

	r.LoadHTMLGlob("web/templates/*")
	// Serve HLS from the configured directory (must be registered before /static to take precedence)
	r.Static("/static/hls", *hlsDir)
	r.Static("/static", "./web/static")

	// Public Routes
	r.GET("/login", func(c *gin.Context) {
		c.HTML(http.StatusOK, "login.html", nil)
	})

	r.POST("/login", func(c *gin.Context) {
		username := c.PostForm("username")
		password := c.PostForm("password")

		user, err := db.Authenticate(username, password)
		if err != nil {
			c.HTML(http.StatusUnauthorized, "login.html", gin.H{"Error": "Invalid credentials"})
			return
		}

		session := sessions.Default(c)
		session.Set("user_id", user.ID)
		session.Save()
		c.Redirect(http.StatusFound, "/")
	})

	r.GET("/logout", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Clear()
		session.Save()
		c.Redirect(http.StatusFound, "/login")
	})

	// Serve recordings (protected if inside protected group, but static middleware runs before)
	// To protect static files properly, we'd need a custom handler or middleware check.
	// For simplicity, let's just protect the dashboard/API.
	r.Static("/recordings", *recDir)

	// Protected Routes
	authorized := r.Group("/")
	authorized.Use(authMiddleware())
	{
		authorized.GET("/", func(c *gin.Context) {
			activeStreams := rtmpSrv.GetActiveStreams()
			keys, _ := db.GetAllStreamKeys()
			// Get the host from the request (for correct RTMP URL display)
			host := c.Request.Host
			if idx := strings.LastIndex(host, ":"); idx != -1 {
				host = host[:idx]
			}
			c.HTML(http.StatusOK, "index.html", gin.H{
				"Streams": activeStreams,
				"Keys":    keys,
				"Host":    host,
			})
		})

		authorized.GET("/recordings-page", func(c *gin.Context) {
			keys, _ := db.GetAllStreamKeys()
			host := c.Request.Host
			if idx := strings.LastIndex(host, ":"); idx != -1 {
				host = host[:idx]
			}
			c.HTML(http.StatusOK, "recordings.html", gin.H{
				"Keys": keys,
				"Host": host,
			})
		})

		authorized.GET("/player/:key", func(c *gin.Context) {
			key := c.Param("key")
			// Get the host from the request (for correct RTMP URL display)
			host := c.Request.Host
			// Strip port if present to get just the hostname/IP
			if idx := strings.LastIndex(host, ":"); idx != -1 {
				host = host[:idx]
			}
			c.HTML(http.StatusOK, "player.html", gin.H{
				"Key":  key,
				"Host": host,
			})
		})

		// API
		api := authorized.Group("/api")
		{
			api.GET("/streams", func(c *gin.Context) {
				activeStreams := rtmpSrv.GetActiveStreams()
				c.JSON(http.StatusOK, gin.H{"streams": activeStreams})
			})

			api.POST("/keys", func(c *gin.Context) {
				var form struct {
					Key         string `json:"key"`
					Description string `json:"description"`
				}
				if err := c.BindJSON(&form); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}

				key := form.Key
				if key == "" {
					key = uuid.New().String()
				}

				sk, err := db.CreateStreamKey(key, form.Description)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
				c.JSON(http.StatusOK, sk)
			})

			// Get all stream keys with summaries
			api.GET("/stream-keys", func(c *gin.Context) {
				keys := rtmpSrv.GetStreamKeys()
				c.JSON(http.StatusOK, gin.H{"keys": keys})
			})

			// Get all recordings for a stream key
			api.GET("/recordings/:key", func(c *gin.Context) {
				key := c.Param("key")
				recordings := rtmpSrv.GetRecordings(key)

				// Check if this key is currently live
				activeStreams := rtmpSrv.GetActiveStreams()
				isLive := false
				for _, s := range activeStreams {
					if s == key {
						isLive = true
						break
					}
				}

				c.JSON(http.StatusOK, gin.H{
					"recordings": recordings,
					"isLive":     isLive,
					"hlsUrl":     fmt.Sprintf("/static/hls/%s/index.m3u8", key),
				})
			})

			// Get all recordings grouped by key
			api.GET("/recordings", func(c *gin.Context) {
				allRecordings := rtmpSrv.GetAllRecordings()
				c.JSON(http.StatusOK, gin.H{"recordings": allRecordings})
			})
		}

		// Download clip from recording
		authorized.GET("/api/download/:key/:filename", func(c *gin.Context) {
			key := c.Param("key")
			filename := c.Param("filename")
			startTimeStr := c.DefaultQuery("start", "0")
			endTimeStr := c.Query("end")

			// Source recording path
			recordingPath := filepath.Join(rtmpSrv.GetRecDir(), key, filename)

			// Check if file exists
			if _, err := os.Stat(recordingPath); os.IsNotExist(err) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Recording not found"})
				return
			}

			// Output filename
			outputName := fmt.Sprintf("%s_clip_%s.mp4", key, startTimeStr)
			c.Header("Content-Disposition", "attachment; filename="+outputName)
			c.Header("Content-Type", "video/mp4")

			// Build ffmpeg args
			args := []string{"-y", "-ss", startTimeStr}

			// Add duration if end time specified
			if endTimeStr != "" {
				args = append(args, "-to", endTimeStr)
			}

			args = append(args,
				"-i", recordingPath,
				"-c", "copy",
				"-f", "mp4",
				"-movflags", "frag_keyframe+empty_moov",
				"pipe:1",
			)

			cmd := exec.Command("ffmpeg", args...)
			cmd.Stdout = c.Writer
			cmd.Stderr = os.Stderr

			if err := cmd.Start(); err != nil {
				log.Println("Download error:", err)
				return
			}
			cmd.Wait()
		})

		// Serve recording files directly
		authorized.Static("/rec", *recDir)
	}

	// Run
	go func() {
		if err := r.Run(":8080"); err != nil {
			log.Fatalf("Web Server failed: %v", err)
		}
	}()

	// Wait for interrupt
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
}

func authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		session := sessions.Default(c)
		userID := session.Get("user_id")
		if userID == nil {
			c.Redirect(http.StatusFound, "/login")
			c.Abort()
			return
		}
		c.Next()
	}
}
