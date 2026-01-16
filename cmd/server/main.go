package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"porteight-camera-ai/internal/db"
	"porteight-camera-ai/internal/stream"

	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func signingSecret() []byte {
	// Must be set in production. When empty, we fall back to API_BEARER_TOKEN for convenience.
	// This still should be a long random value.
	sec := os.Getenv("SIGNING_SECRET")
	if sec == "" {
		sec = os.Getenv("API_BEARER_TOKEN")
	}
	return []byte(sec)
}

func signPath(method, path string, exp int64) string {
	mac := hmac.New(sha256.New, signingSecret())
	// Canonical form: METHOD\nPATH\nEXP
	_, _ = mac.Write([]byte(method))
	_, _ = mac.Write([]byte("\n"))
	_, _ = mac.Write([]byte(path))
	_, _ = mac.Write([]byte("\n"))
	_, _ = mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

func validateSignedRequest(c *gin.Context) bool {
	expStr := c.Query("exp")
	sig := c.Query("sig")
	if expStr == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > exp {
		return false
	}
	if len(signingSecret()) == 0 {
		return false
	}
	expected := signPath(c.Request.Method, c.Request.URL.Path, exp)
	return hmac.Equal([]byte(expected), []byte(sig))
}

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
	webDirEnv := os.Getenv("WEB_DIR")
	if webDirEnv == "" {
		webDirEnv = "./web"
	}
	webDir := flag.String("web", webDirEnv, "directory containing web/templates and web/static")
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

	// CORS to allow dashboard calls (credentials/Bearer)
	allowedOriginsEnv := os.Getenv("ALLOWED_ORIGINS")
	allowedOrigins := []string{"http://localhost:3000", "https://suvidhi.porteight.io"}
	if allowedOriginsEnv != "" {
		allowedOrigins = strings.Split(allowedOriginsEnv, ",")
	}
	r.Use(cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: true,
	}))

	// Session Store
	// IMPORTANT: cookie.NewStore uses gorilla/securecookie. The auth key should be 32+ bytes.
	// If it's too short, session.Save() can fail and you won't be able to login.
	authKey := os.Getenv("SESSION_AUTH_KEY")
	encKey := os.Getenv("SESSION_ENC_KEY") // optional (16/24/32 bytes) for encryption
	if authKey == "" {
		// Keep a fallback for local dev, but strongly recommend setting SESSION_AUTH_KEY in production.
		authKey = "dev-only-please-set-SESSION_AUTH_KEY-to-a-32+-byte-secret"
		log.Println("WARNING: SESSION_AUTH_KEY not set; using an insecure dev default. Set SESSION_AUTH_KEY in production.")
	}
	var store sessions.Store
	if encKey != "" {
		store = cookie.NewStore([]byte(authKey), []byte(encKey))
	} else {
		store = cookie.NewStore([]byte(authKey))
	}

	// Cookie settings (tuned for "IP:8080 over HTTP" by default)
	secureCookie := false
	if v := os.Getenv("SESSION_SECURE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			secureCookie = b
		}
	}
	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 30, // 30 days
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secureCookie, // set true only when serving over HTTPS
	})
	r.Use(sessions.Sessions("mysession", store))

	r.LoadHTMLGlob(filepath.Join(*webDir, "templates", "*"))
	// Serve HLS from the configured directory
	r.Static("/hls", *hlsDir)

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
		if err := session.Save(); err != nil {
			log.Printf("Failed to save session: %v", err)
			c.HTML(http.StatusInternalServerError, "login.html", gin.H{"Error": "Login failed (session error). Check server logs."})
			return
		}
		c.Redirect(http.StatusFound, "/")
	})

	r.GET("/logout", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Clear()
		session.Save()
		c.Redirect(http.StatusFound, "/login")
	})

	// Signed public media endpoints (no cookies/headers required).
	// Used by external dashboards where <video> cannot attach Authorization headers.
	r.GET("/public/rec/:key/:filename", func(c *gin.Context) {
		if !validateSignedRequest(c) {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		key := c.Param("key")
		filename := filepath.Base(c.Param("filename"))
		c.File(filepath.Join(*recDir, key, filename))
	})
	r.GET("/public/download/:key/:filename", func(c *gin.Context) {
		if !validateSignedRequest(c) {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		key := c.Param("key")
		filename := filepath.Base(c.Param("filename"))
		startTimeStr := c.DefaultQuery("start", "0")
		endTimeStr := c.Query("end")

		recordingPath := filepath.Join(rtmpSrv.GetRecDir(), key, filename)
		if _, err := os.Stat(recordingPath); os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Recording not found"})
			return
		}

		outputName := fmt.Sprintf("%s_clip_%s.mp4", key, startTimeStr)
		c.Header("Content-Disposition", "attachment; filename="+outputName)
		c.Header("Content-Type", "video/mp4")

		args := []string{"-y", "-ss", startTimeStr}
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
		_ = cmd.Wait()
	})

	// Serve recordings (legacy static, keep for admin UI access)
	r.Static("/recordings", *recDir)

	// Protected Routes (admin UI)
	authorized := r.Group("/")
	authorized.Use(authMiddleware())
	{
		authorized.GET("/", func(c *gin.Context) {
			activeStreams := rtmpSrv.GetActiveStreams()
			keys, _ := db.GetAllStreamKeys()
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
			host := c.Request.Host
			if idx := strings.LastIndex(host, ":"); idx != -1 {
				host = host[:idx]
			}
			c.HTML(http.StatusOK, "player.html", gin.H{
				"Key":  key,
				"Host": host,
			})
		})
	}

	// API (Bearer token OR session) for external dashboard
	api := r.Group("/api")
	api.Use(apiAuthMiddleware())
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

		api.GET("/stream-keys", func(c *gin.Context) {
			keys := rtmpSrv.GetStreamKeys()
			c.JSON(http.StatusOK, gin.H{"keys": keys})
		})

		api.GET("/recordings/:key", func(c *gin.Context) {
			key := c.Param("key")
			recordings := rtmpSrv.GetRecordings(key)
			activeStreams := rtmpSrv.GetActiveStreams()
			isLive := false
			for _, s := range activeStreams {
				if s == key {
					isLive = true
					break
				}
			}
			// Attach signed playback/download URLs for dashboard video/download usage.
			exp := time.Now().Add(15 * time.Minute).Unix()
			enriched := make([]gin.H, 0, len(recordings))
			for _, r := range recordings {
				recPath := fmt.Sprintf("/public/rec/%s/%s", r.Key, r.Filename)
				dlPath := fmt.Sprintf("/public/download/%s/%s", r.Key, r.Filename)
				enriched = append(enriched, gin.H{
					"key":         r.Key,
					"filename":    r.Filename,
					"path":        r.Path,
					"startTime":   r.StartTime,
					"endTime":     r.EndTime,
					"size":        r.Size,
					"duration":    r.Duration,
					"isLive":      r.IsLive,
					"playbackUrl": fmt.Sprintf("%s?exp=%d&sig=%s", recPath, exp, signPath("GET", recPath, exp)),
					"downloadUrl": fmt.Sprintf("%s?exp=%d&sig=%s", dlPath, exp, signPath("GET", dlPath, exp)),
				})
			}

			c.JSON(http.StatusOK, gin.H{
				"recordings": enriched,
				"isLive":     isLive,
				"hlsUrl":     fmt.Sprintf("/hls/%s/index.m3u8", key),
			})
		})

		api.GET("/recordings", func(c *gin.Context) {
			allRecordings := rtmpSrv.GetAllRecordings()
			c.JSON(http.StatusOK, gin.H{"recordings": allRecordings})
		})

		api.GET("/download/:key/:filename", func(c *gin.Context) {
			key := c.Param("key")
			filename := c.Param("filename")
			startTimeStr := c.DefaultQuery("start", "0")
			endTimeStr := c.Query("end")

			recordingPath := filepath.Join(rtmpSrv.GetRecDir(), key, filename)
			if _, err := os.Stat(recordingPath); os.IsNotExist(err) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Recording not found"})
				return
			}

			outputName := fmt.Sprintf("%s_clip_%s.mp4", key, startTimeStr)
			c.Header("Content-Disposition", "attachment; filename="+outputName)
			c.Header("Content-Type", "video/mp4")

			args := []string{"-y", "-ss", startTimeStr}
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

		api.Static("/rec", *recDir)
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

// apiAuthMiddleware allows either a valid Bearer token or an authenticated session.
func apiAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		apiToken := os.Getenv("API_BEARER_TOKEN")
		authHeader := c.GetHeader("Authorization")

		if apiToken != "" && strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == apiToken {
				c.Next()
				return
			}
		}

		// Fallback to session auth (admin cookies)
		session := sessions.Default(c)
		if session.Get("user_id") != nil {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
	}
}
