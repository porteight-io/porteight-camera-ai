package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"porteight-camera-ai/internal/db"
	"porteight-camera-ai/internal/storage"
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

	// Optional: S3 upload for archived HLS recordings + S3-backed playback
	var s3Store *storage.S3Store
	var s3UpMu sync.Mutex
	s3UploadCancels := map[string]context.CancelFunc{} // sessionID -> cancel
	{
		s3Bucket := os.Getenv("S3_BUCKET")
		if s3Bucket != "" {
			st, err := storage.NewS3Store(context.Background(), storage.S3Config{
				Region:     os.Getenv("S3_REGION"),
				Bucket:     s3Bucket,
				Prefix:     os.Getenv("S3_PREFIX"),
				SSE:        os.Getenv("S3_SSE"),        // "AES256" or "aws:kms"
				KMSKeyID:   os.Getenv("S3_KMS_KEY_ID"), // optional unless you require a specific key
				PresignTTL: 5 * time.Minute,
			})
			if err != nil {
				log.Fatalf("Failed to init S3 store: %v", err)
			}
			s3Store = st
			log.Printf("S3 enabled bucket=%s region=%s prefix=%s sse=%s", os.Getenv("S3_BUCKET"), os.Getenv("S3_REGION"), os.Getenv("S3_PREFIX"), os.Getenv("S3_SSE"))

			// Incremental upload (as files are produced).
			// This makes recordings available in S3 while the stream is still running.
			rtmpSrv.SetOnRecordingStart(func(key string, session string, localDir string) {
				// Index early so the UI can list sessions even if local files are later purged.
				start, err := time.ParseInLocation("2006-01-02_15-04-05", session, time.Local)
				if err != nil {
					start = time.Now()
				}
				_ = db.UpsertHLSRecordingSession(&db.HLSRecordingSession{
					StreamKey: key,
					Session:   session,
					Storage:   "local",
					LocalDir:  localDir,
					S3Bucket:  "",
					S3Prefix:  "",
					Uploaded:  false,
					UploadErr: "",
					StartTime: start,
					EndTime:   start,
					Duration:  0,
					SizeBytes: 0,
				})

				sessionID := key + "/" + session
				ctx, cancel := context.WithCancel(context.Background())
				s3UpMu.Lock()
				// cancel any existing uploader for same sessionID (shouldn't happen)
				if old, ok := s3UploadCancels[sessionID]; ok && old != nil {
					old()
				}
				s3UploadCancels[sessionID] = cancel
				s3UpMu.Unlock()

				go func() {
					interval := 2 * time.Second
					if v := os.Getenv("S3_UPLOAD_INTERVAL_SECONDS"); v != "" {
						if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 60 {
							interval = time.Duration(n) * time.Second
						}
					}

					prefix := s3Store.RecordingPrefixForSession(key, session) // includes optional global prefix
					log.Printf("S3 incremental upload started key=%s session=%s s3Prefix=%s", key, session, prefix)

					type fileState struct {
						size int64
						mod  time.Time
					}
					uploaded := map[string]bool{}      // rel -> done
					lastSeen := map[string]fileState{} // rel -> last stat
					lastPlaylistUpload := time.Time{}

					ticker := time.NewTicker(interval)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							log.Printf("S3 incremental upload stopped key=%s session=%s", key, session)
							return
						case <-ticker.C:
							entries, err := os.ReadDir(localDir)
							if err != nil {
								continue
							}

							for _, e := range entries {
								if e.IsDir() {
									continue
								}
								name := e.Name()
								if strings.HasSuffix(name, ".tmp") {
									continue
								}
								if name != "index.m3u8" && !strings.HasSuffix(strings.ToLower(name), ".ts") {
									continue
								}
								if uploaded[name] && name != "index.m3u8" {
									continue
								}

								fullPath := filepath.Join(localDir, name)
								fi, err := os.Stat(fullPath)
								if err != nil || fi.Size() == 0 {
									continue
								}

								prev, ok := lastSeen[name]
								lastSeen[name] = fileState{size: fi.Size(), mod: fi.ModTime()}
								// Wait for file to "stabilize" across 2 scans (avoid uploading while still being written)
								if ok && prev.size == fi.Size() && !fi.ModTime().After(prev.mod) {
									// stable
								} else {
									continue
								}

								// Upload playlist at a slower cadence (but still frequently)
								if name == "index.m3u8" {
									if time.Since(lastPlaylistUpload) < interval {
										continue
									}
								}

								objKey := prefix + "/" + name
								if err := s3Store.UploadFile(ctx, fullPath, objKey); err != nil {
									log.Printf("S3 incremental upload failed key=%s session=%s file=%s err=%v", key, session, name, err)
									continue
								}

								if name == "index.m3u8" {
									lastPlaylistUpload = time.Now()
								} else {
									uploaded[name] = true
									// Optional: delete local segments after upload (S3-only recordings).
									if v := os.Getenv("S3_DELETE_LOCAL_SEGMENTS"); v != "" {
										if b, err := strconv.ParseBool(v); err == nil && b {
											_ = os.Remove(fullPath)
										}
									}
								}
							}
						}
					}
				}()
			})

			// Upload only the per-session recordings directory to S3 (recordings/<key>/<session>/...)
			rtmpSrv.SetOnRecordingComplete(func(key string, session string, localDir string) {
				// Stop incremental uploader (if running) and do a final sync upload to guarantee completeness.
				sessionID := key + "/" + session
				s3UpMu.Lock()
				if cancel, ok := s3UploadCancels[sessionID]; ok && cancel != nil {
					cancel()
				}
				delete(s3UploadCancels, sessionID)
				s3UpMu.Unlock()

				log.Printf("S3 upload starting key=%s session=%s dir=%s", key, session, localDir)
				// Index in DB so it remains discoverable even if local files are later deleted.
				start, err := time.ParseInLocation("2006-01-02_15-04-05", session, time.Local)
				if err != nil {
					start = time.Now()
				}
				// Best-effort size (sum files)
				var size int64
				_ = filepath.WalkDir(localDir, func(p string, d os.DirEntry, err error) error {
					if err != nil || d.IsDir() {
						return nil
					}
					if fi, err := d.Info(); err == nil {
						size += fi.Size()
					}
					return nil
				})
				_ = db.UpsertHLSRecordingSession(&db.HLSRecordingSession{
					StreamKey: key,
					Session:   session,
					Storage:   "local",
					LocalDir:  localDir,
					StartTime: start,
					EndTime:   start, // updated after upload or by later metadata pass
					Duration:  0,
					SizeBytes: size,
				})

				destPrefix := fmt.Sprintf("recordings/%s/%s", key, session)
				fullPrefix, err := s3Store.UploadDir(context.Background(), localDir, destPrefix)
				if err != nil {
					log.Printf("S3 upload failed key=%s session=%s err=%v", key, session, err)
					_ = db.MarkHLSRecordingUploaded(key, session, false, err.Error(), "local", "", "")
					return
				}
				_ = db.MarkHLSRecordingUploaded(key, session, true, "", "s3", s3Store.Bucket(), fullPrefix)
				log.Printf("S3 upload complete key=%s session=%s s3=%s/%s", key, session, s3Store.Bucket(), fullPrefix)

				// Optional: remove the whole local session folder after successful upload.
				if v := os.Getenv("S3_DELETE_LOCAL_SESSION_ON_COMPLETE"); v != "" {
					if b, err := strconv.ParseBool(v); err == nil && b {
						_ = os.RemoveAll(localDir)
					}
				}
			})
		}
	}

	go func() {
		if err := rtmpSrv.Start(); err != nil {
			log.Fatalf("RTMP Server failed: %v", err)
		}
	}()

	// Initialize Web Server
	r := gin.Default()

	// IMPORTANT: Do NOT apply CORS globally.
	// CORS preflight/origin checks can break browser form posts to /login.
	// We apply CORS only on the /api group further below.

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
	// Static files.
	//
	// Frontend templates expect:
	// - /static/hls/<key>/index.m3u8
	// - /static/hls/<key>/seg_*.ts
	//
	// But HLS_DIR may point outside ./web/static/hls (e.g. PERSIST_DIR/hls), so we mount it explicitly.
	r.Static("/static/hls", *hlsDir)
	// Backwards compat: some clients use /hls/<key>/index.m3u8 (API currently returns this)
	r.Static("/hls", *hlsDir)
	// Other static assets (if any) under web/static.
	// NOTE: We cannot mount /static/* because it would conflict with /static/hls/*.
	// Serve them under /assets instead.
	r.Static("/assets", filepath.Join(*webDir, "static"))

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
	// Admin UI expects /rec/<key>/<filename>
	r.Static("/rec", *recDir)

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
	allowedOriginsEnv := os.Getenv("ALLOWED_ORIGINS")
	allowedOrigins := []string{"http://localhost:3000", "https://suvidhi.porteight.io"}
	if allowedOriginsEnv != "" {
		allowedOrigins = strings.Split(allowedOriginsEnv, ",")
	}
	api := r.Group("/api")
	api.Use(cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: true,
	}))
	api.Use(apiAuthMiddleware())
	{
		parseHLSDurationFromM3U8 := func(m3u8 string) float64 {
			var total float64
			for _, line := range strings.Split(m3u8, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "#EXTINF:") {
					continue
				}
				rest := strings.TrimPrefix(line, "#EXTINF:")
				if idx := strings.Index(rest, ","); idx != -1 {
					rest = rest[:idx]
				}
				rest = strings.TrimSpace(rest)
				if rest == "" {
					continue
				}
				if v, err := strconv.ParseFloat(rest, 64); err == nil && v > 0 {
					total += v
				}
			}
			return total
		}

		api.GET("/s3/status", func(c *gin.Context) {
			if s3Store == nil {
				c.JSON(http.StatusOK, gin.H{"enabled": false})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"enabled": true,
				"bucket":  os.Getenv("S3_BUCKET"),
				"region":  os.Getenv("S3_REGION"),
				"prefix":  os.Getenv("S3_PREFIX"),
				"sse":     os.Getenv("S3_SSE"),
			})
		})

		// Upload any existing local sessions for a key (handy for initial testing/backfills).
		api.POST("/s3/backfill/:key", func(c *gin.Context) {
			if s3Store == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "s3 not configured"})
				return
			}
			key := c.Param("key")
			baseDir := filepath.Join(*hlsDir, key, "recordings")
			entries, err := os.ReadDir(baseDir)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "no local recordings found"})
				return
			}
			type resItem struct {
				Session string `json:"session"`
				OK      bool   `json:"ok"`
				Error   string `json:"error,omitempty"`
			}
			results := make([]resItem, 0)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				session := e.Name()
				localDir := filepath.Join(baseDir, session)
				if _, err := os.Stat(filepath.Join(localDir, "index.m3u8")); err != nil {
					continue
				}
				destPrefix := fmt.Sprintf("recordings/%s/%s", key, session)
				_, upErr := s3Store.UploadDir(c.Request.Context(), localDir, destPrefix)
				if upErr != nil {
					results = append(results, resItem{Session: session, OK: false, Error: upErr.Error()})
					continue
				}
				results = append(results, resItem{Session: session, OK: true})
			}
			c.JSON(http.StatusOK, gin.H{"results": results})
		})

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
			// In S3-only mode we may delete local recordings, so rtmpSrv.GetStreamKeys() would go empty.
			// If S3 is configured, derive the sidebar keys list from DB-indexed sessions instead.
			if s3Store != nil {
				type aggRow struct {
					StreamKey       string  `gorm:"column:stream_key"`
					TotalRecordings int64   `gorm:"column:total_recordings"`
					TotalDuration   float64 `gorm:"column:total_duration"`
					TotalSize       int64   `gorm:"column:total_size"`
					EarliestUnix    int64   `gorm:"column:earliest_unix"`
					LatestUnix      int64   `gorm:"column:latest_unix"`
				}
				var rows []aggRow
				_ = db.DB.Model(&db.HLSRecordingSession{}).
					// SQLite stores time as TEXT by default; use strftime('%s', ...) to get unix seconds.
					Select("stream_key as stream_key, count(*) as total_recordings, coalesce(sum(duration),0) as total_duration, coalesce(sum(size_bytes),0) as total_size, coalesce(min(strftime('%s', start_time)),0) as earliest_unix, coalesce(max(strftime('%s', end_time)),0) as latest_unix").
					Group("stream_key").
					Order("earliest_unix asc").
					Scan(&rows).Error

				keys := make([]gin.H, 0, len(rows))
				for _, r := range rows {
					isLive := rtmpSrv.IsStreamLive(r.StreamKey)
					keys = append(keys, gin.H{
						"key":             r.StreamKey,
						"totalRecordings": r.TotalRecordings,
						"totalDuration":   r.TotalDuration,
						"totalSize":       r.TotalSize,
						"isLive":          isLive,
						"earliestRecord":  r.EarliestUnix,
						"latestRecord":    r.LatestUnix,
					})
				}
				c.JSON(http.StatusOK, gin.H{"keys": keys})
				return
			}

			keys := rtmpSrv.GetStreamKeys()
			c.JSON(http.StatusOK, gin.H{"keys": keys})
		})

		api.GET("/recordings/:key", func(c *gin.Context) {
			key := c.Param("key")
			// Local scan (works when recordings are still on disk)
			recordings := rtmpSrv.GetRecordings(key)
			activeStreams := rtmpSrv.GetActiveStreams()
			isLive := false
			for _, s := range activeStreams {
				if s == key {
					isLive = true
					break
				}
			}
			// Merge local scan with DB-indexed sessions so "S3-only" mode still shows recordings.
			type recOut struct {
				Key       string
				Filename  string
				Playlist  string
				StartTime int64
				EndTime   int64
				Size      int64
				Duration  float64
				IsLive    bool
			}
			bySession := map[string]recOut{}

			for _, r := range recordings {
				playlist := r.Playlist
				if s3Store != nil && !r.IsLive {
					playlist = fmt.Sprintf("/api/hls/%s/recordings/%s/index.m3u8", r.Key, r.Filename)
				}
				bySession[r.Filename] = recOut{
					Key:       r.Key,
					Filename:  r.Filename,
					Playlist:  playlist,
					StartTime: r.StartTime,
					EndTime:   r.EndTime,
					Size:      r.Size,
					Duration:  r.Duration,
					IsLive:    r.IsLive,
				}
			}

			if s3Store != nil {
				sessions, _ := db.ListHLSRecordingSessions(key)
				for _, s := range sessions {
					if _, ok := bySession[s.Session]; ok {
						continue
					}
					startTs := s.StartTime.Unix()
					dur := s.Duration
					if dur <= 0 {
						// Try to compute duration from S3 playlist (cheap: small file)
						objPrefix := s3Store.RecordingPrefixForSession(key, s.Session)
						body, err := s3Store.GetObject(c.Request.Context(), objPrefix+"/index.m3u8")
						if err == nil {
							b, _ := io.ReadAll(body)
							_ = body.Close()
							dur = parseHLSDurationFromM3U8(string(b))
						}
					}
					endTs := startTs + int64(dur)
					bySession[s.Session] = recOut{
						Key:       key,
						Filename:  s.Session,
						Playlist:  fmt.Sprintf("/api/hls/%s/recordings/%s/index.m3u8", key, s.Session),
						StartTime: startTs,
						EndTime:   endTs,
						Size:      s.SizeBytes,
						Duration:  dur,
						IsLive:    false,
					}
				}
			}

			enriched := make([]gin.H, 0, len(bySession))
			for _, v := range bySession {
				enriched = append(enriched, gin.H{
					"key":       v.Key,
					"filename":  v.Filename,
					"playlist":  v.Playlist,
					"startTime": v.StartTime,
					"endTime":   v.EndTime,
					"size":      v.Size,
					"duration":  v.Duration,
					"isLive":    v.IsLive,
				})
			}

			c.JSON(http.StatusOK, gin.H{
				"recordings": enriched,
				"isLive":     isLive,
				// live HLS (rolling)
				"hlsUrl": fmt.Sprintf("/hls/%s/index.m3u8", key),
			})
		})

		// Secure HLS playlist proxy for archived recordings.
		// - If S3 is configured: reads index.m3u8 + segments from S3 (private bucket) and streams segments through backend.
		// - If S3 is not configured or objects missing: falls back to local disk (HLS_DIR).
		//
		// Query:
		// - direct=1 : playlist segment URIs are rewritten to presigned S3 URLs (requires S3 + S3 CORS),
		//              otherwise default is proxying segments via backend (no S3 CORS needed).
		api.GET("/hls/:key/recordings/:session/index.m3u8", func(c *gin.Context) {
			key := c.Param("key")
			session := c.Param("session")
			direct := c.Query("direct") == "1"

			localPlaylist := filepath.Join(*hlsDir, key, "recordings", session, "index.m3u8")

			var playlistBytes []byte
			var fromS3 bool
			if s3Store != nil {
				objPrefix := s3Store.RecordingPrefixForSession(key, session)
				body, err := s3Store.GetObject(c.Request.Context(), objPrefix+"/index.m3u8")
				if err == nil {
					defer body.Close()
					playlistBytes, _ = io.ReadAll(body)
					fromS3 = true
				}
			}
			if len(playlistBytes) == 0 {
				b, err := os.ReadFile(localPlaylist)
				if err != nil {
					c.JSON(http.StatusNotFound, gin.H{"error": "playlist not found"})
					return
				}
				playlistBytes = b
			}

			lines := strings.Split(string(playlistBytes), "\n")
			outLines := make([]string, 0, len(lines))
			for _, line := range lines {
				trim := strings.TrimSpace(line)
				if trim == "" || strings.HasPrefix(trim, "#") {
					outLines = append(outLines, line)
					continue
				}

				// Non-comment line is usually a segment path like seg_000001.ts
				if fromS3 && s3Store != nil {
					if direct {
						objPrefix := s3Store.RecordingPrefixForSession(key, session)
						url, err := s3Store.PresignGet(c.Request.Context(), objPrefix+"/"+trim)
						if err != nil {
							c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to sign segment"})
							return
						}
						outLines = append(outLines, url)
					} else {
						outLines = append(outLines, fmt.Sprintf("/api/hls/%s/recordings/%s/%s", key, session, trim))
					}
				} else {
					// Local fallback (served by /static/hls)
					outLines = append(outLines, fmt.Sprintf("/static/hls/%s/recordings/%s/%s", key, session, trim))
				}
			}

			c.Header("Content-Type", "application/vnd.apple.mpegurl")
			c.String(http.StatusOK, strings.Join(outLines, "\n"))
		})

		// Segment proxy for archived recordings (S3 first, then local fallback).
		api.GET("/hls/:key/recordings/:session/:segment", func(c *gin.Context) {
			key := c.Param("key")
			session := c.Param("session")
			segment := filepath.Base(c.Param("segment"))
			if segment == "" {
				c.Status(http.StatusBadRequest)
				return
			}

			// Prefer S3 if configured
			if s3Store != nil {
				objPrefix := s3Store.RecordingPrefixForSession(key, session)
				body, err := s3Store.GetObject(c.Request.Context(), objPrefix+"/"+segment)
				if err == nil {
					defer body.Close()
					// Best-effort content type
					if strings.HasSuffix(strings.ToLower(segment), ".ts") {
						c.Header("Content-Type", "video/MP2T")
					}
					_, _ = io.Copy(c.Writer, body)
					return
				}
			}

			// Local fallback
			localPath := filepath.Join(*hlsDir, key, "recordings", session, segment)
			if _, err := os.Stat(localPath); err != nil {
				c.Status(http.StatusNotFound)
				return
			}
			c.File(localPath)
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

		// Export a single file that may span multiple recordings.
		//
		// Query params:
		// - key: stream key
		// - segments: JSON array, URL-encoded:
		//   [{ "filename": "...mp4", "start": 0, "end": 123.4, "duration": 456.7 }, ...]
		//   start/end/duration are seconds (floats ok). end may equal duration for full segment.
		api.GET("/export", func(c *gin.Context) {
			key := c.Query("key")
			segmentsJSON := c.Query("segments")
			if key == "" || segmentsJSON == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "missing key or segments"})
				return
			}

			type segReq struct {
				Playlist string  `json:"playlist"`
				Start    float64 `json:"start"`
				End      float64 `json:"end"`
				Duration float64 `json:"duration"`
			}
			var segs []segReq
			if err := json.Unmarshal([]byte(segmentsJSON), &segs); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid segments json"})
				return
			}
			if len(segs) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "no segments"})
				return
			}

			// Validate and build absolute paths
			tmpDir, err := os.MkdirTemp("", "porteight-export-*")
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create temp dir"})
				return
			}
			defer os.RemoveAll(tmpDir)

			concatListPath := filepath.Join(tmpDir, "list.txt")
			var listLines []string

			escapeConcatPath := func(p string) string {
				// concat demuxer list uses single quotes; escape any single quote characters.
				// Also keep the path as-is (it may contain spaces).
				return strings.ReplaceAll(p, "'", "'\\''")
			}

			// Create per-segment (trimmed) files when needed, otherwise use the original file.
			for i, sreq := range segs {
				pl := sreq.Playlist
				if pl == "" || !strings.HasSuffix(strings.ToLower(pl), ".m3u8") || !strings.HasPrefix(pl, "/static/hls/") {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid playlist"})
					return
				}

				// Map URL playlist -> filesystem path under configured HLS_DIR
				rel := strings.TrimPrefix(pl, "/static/hls/")
				rel = filepath.Clean(rel)
				if strings.HasPrefix(rel, "..") {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid playlist path"})
					return
				}
				inPath := filepath.Join(*hlsDir, rel)
				if _, err := os.Stat(inPath); err != nil {
					c.JSON(http.StatusNotFound, gin.H{"error": "playlist not found"})
					return
				}

				start := sreq.Start
				end := sreq.End
				if start < 0 {
					start = 0
				}
				if end < 0 {
					end = 0
				}
				if end > 0 && end < start {
					c.JSON(http.StatusBadRequest, gin.H{"error": "segment end must be >= start"})
					return
				}

				needsTrim := start > 0.01
				if sreq.Duration > 0 && end > 0 && end < (sreq.Duration-0.01) {
					needsTrim = true
				}

				outPath := inPath
				if needsTrim {
					// Trim this HLS playlist into a temp MP4 part, then concat parts.
					outPath = filepath.Join(tmpDir, fmt.Sprintf("seg_%03d.mp4", i))
					trimDur := 0.0
					if end > 0 {
						trimDur = end - start
						if trimDur <= 0.05 {
							c.JSON(http.StatusBadRequest, gin.H{"error": "segment duration too small"})
							return
						}
					}
					argsCopy := []string{
						"-y",
						"-ss", fmt.Sprintf("%.3f", start),
						"-i", inPath,
					}
					if trimDur > 0 {
						argsCopy = append(argsCopy, "-t", fmt.Sprintf("%.3f", trimDur))
					}
					argsCopy = append(argsCopy,
						// HLS(mpegts) -> mp4 needs AAC bitstream filter when copying audio
						"-c", "copy",
						"-bsf:a", "aac_adtstoasc",
						"-avoid_negative_ts", "make_zero",
						"-movflags", "+faststart",
						outPath,
					)
					cmdCopy := exec.Command("ffmpeg", argsCopy...)
					out, err := cmdCopy.CombinedOutput()
					if err != nil {
						log.Printf("ffmpeg trim (copy) failed: %v output=%s", err, string(out))
						// fallback: re-encode this part
						argsX := []string{
							"-y",
							"-ss", fmt.Sprintf("%.3f", start),
							"-i", inPath,
						}
						if trimDur > 0 {
							argsX = append(argsX, "-t", fmt.Sprintf("%.3f", trimDur))
						}
						argsX = append(argsX,
							"-fflags", "+genpts",
							"-c:v", "libx265",
							"-preset", "veryfast",
							"-crf", "28",
							"-tag:v", "hvc1",
							"-c:a", "aac",
							"-b:a", "96k",
							"-ar", "48000",
							"-max_muxing_queue_size", "2048",
							"-movflags", "+faststart",
							outPath,
						)
						cmdX := exec.Command("ffmpeg", argsX...)
						out2, err2 := cmdX.CombinedOutput()
						if err2 != nil {
							log.Printf("ffmpeg trim (xcode) failed: %v output=%s", err2, string(out2))
							c.JSON(http.StatusInternalServerError, gin.H{"error": "ffmpeg trim failed"})
							return
						}
					}

					if fi, err := os.Stat(outPath); err != nil || fi.Size() == 0 {
						log.Printf("ffmpeg trim produced empty file: %s", outPath)
						c.JSON(http.StatusInternalServerError, gin.H{"error": "ffmpeg trim produced empty output"})
						return
					}
				} else {
					// No trim needed; still convert this playlist to MP4 for concat (more reliable).
					outPath = filepath.Join(tmpDir, fmt.Sprintf("seg_%03d.mp4", i))
					args := []string{
						"-y",
						"-i", inPath,
						"-c", "copy",
						"-bsf:a", "aac_adtstoasc",
						"-movflags", "+faststart",
						outPath,
					}
					cmd := exec.Command("ffmpeg", args...)
					out, err := cmd.CombinedOutput()
					if err != nil {
						log.Printf("ffmpeg part (copy) failed: %v output=%s", err, string(out))
						// fallback: re-encode full part
						argsX := []string{
							"-y",
							"-i", inPath,
							"-fflags", "+genpts",
							"-c:v", "libx265",
							"-preset", "veryfast",
							"-crf", "28",
							"-tag:v", "hvc1",
							"-c:a", "aac",
							"-b:a", "96k",
							"-ar", "48000",
							"-max_muxing_queue_size", "2048",
							"-movflags", "+faststart",
							outPath,
						}
						cmdX := exec.Command("ffmpeg", argsX...)
						out2, err2 := cmdX.CombinedOutput()
						if err2 != nil {
							log.Printf("ffmpeg part (xcode) failed: %v output=%s", err2, string(out2))
							c.JSON(http.StatusInternalServerError, gin.H{"error": "ffmpeg export failed"})
							return
						}
					}
					if fi, err := os.Stat(outPath); err != nil || fi.Size() == 0 {
						log.Printf("ffmpeg part produced empty file: %s", outPath)
						c.JSON(http.StatusInternalServerError, gin.H{"error": "ffmpeg export produced empty output"})
						return
					}
				}

				// Concat demuxer list format
				listLines = append(listLines, fmt.Sprintf("file '%s'\n", escapeConcatPath(outPath)))
			}

			if err := os.WriteFile(concatListPath, []byte(strings.Join(listLines, "")), 0644); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write concat list"})
				return
			}

			downloadName := c.DefaultQuery("name", fmt.Sprintf("%s_export.mp4", key))
			downloadName = filepath.Base(downloadName)
			if downloadName == "" {
				downloadName = fmt.Sprintf("%s_export.mp4", key)
			}

			// Concat into a single MP4 (file output first), stream-copy (fast).
			// We intentionally write to a temp file and verify size > 0 before sending,
			// otherwise browsers can "download" a 0-byte file when ffmpeg fails.
			outFile := filepath.Join(tmpDir, "export.mp4")
			argsCopy := []string{
				"-y",
				"-f", "concat",
				"-safe", "0",
				"-i", concatListPath,
				"-c", "copy",
				"-tag:v", "hvc1",
				"-movflags", "+faststart",
				outFile,
			}
			cmdCopy := exec.Command("ffmpeg", argsCopy...)
			out, err := cmdCopy.CombinedOutput()
			if err != nil {
				// Fallback: re-encode the concat output (more robust across timestamp/codec quirks).
				log.Printf("ffmpeg export (copy) failed: %v output=%s", err, string(out))

				argsXcode := []string{
					"-y",
					"-f", "concat",
					"-safe", "0",
					"-i", concatListPath,
					// Generate fresh timestamps on output to avoid non-monotonic issues.
					"-fflags", "+genpts",
					// Video: HEVC
					"-c:v", "libx265",
					"-preset", "veryfast",
					"-crf", "28",
					"-tag:v", "hvc1",
					// Audio: AAC (normalize sample rate for compatibility)
					"-c:a", "aac",
					"-b:a", "96k",
					"-ar", "48000",
					// Helpful for some weird inputs
					"-max_muxing_queue_size", "2048",
					"-movflags", "+faststart",
					outFile,
				}
				cmdXcode := exec.Command("ffmpeg", argsXcode...)
				out2, err2 := cmdXcode.CombinedOutput()
				if err2 != nil {
					log.Printf("ffmpeg export (xcode) failed: %v output=%s", err2, string(out2))
					c.JSON(http.StatusInternalServerError, gin.H{"error": "ffmpeg export failed"})
					return
				}
			}
			if fi, err := os.Stat(outFile); err != nil || fi.Size() == 0 {
				log.Printf("ffmpeg export produced empty file: %s", outFile)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "ffmpeg export produced empty output"})
				return
			}

			c.FileAttachment(outFile, downloadName)
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
