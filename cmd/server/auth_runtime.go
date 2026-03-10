package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	nextauth "porteight-camera-ai/internal/auth"
	"porteight-camera-ai/internal/db"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	cameraAccessTokenCookie = "camera_access_token"
	adminSessionCookie      = "camera_admin_session"
	authContextUserKey      = "camera_auth_user"
	adminSessionTTL         = 24 * time.Hour
)

type cameraAuthUser struct {
	Token        string
	Claims       *nextauth.TokenClaims
	AllowEntry   *db.CameraAccessUser
	IsSuperAdmin bool
	AdminUser    string
}

type authFailure struct {
	Status  int
	Message string
}

// ── Secrets ──────────────────────────────────────────────────────────────────

func nextAuthSecret() string {
	return os.Getenv("NEXTAUTH_SECRET")
}

func adminSessionSecret() string {
	if s := os.Getenv("ADMIN_SESSION_SECRET"); s != "" {
		return s
	}
	return os.Getenv("NEXTAUTH_SECRET") + "-camera-admin-session"
}

func authCookieSecure() bool {
	if value := os.Getenv("AUTH_COOKIE_SECURE"); value != "" {
		return strings.EqualFold(value, "true")
	}
	if value := os.Getenv("SESSION_SECURE"); value != "" {
		return strings.EqualFold(value, "true")
	}
	return false
}

// ── Admin session token (stateless HMAC-signed) ───────────────────────────────
//
// Token format (base64url): <json-payload>.<hmac-signature>
// Payload: {"sub": username, "exp": unix-timestamp}

type adminSessionPayload struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

func createAdminSessionToken(username string) (string, error) {
	secret := adminSessionSecret()
	if secret == "" {
		return "", errors.New("admin session secret is not configured")
	}

	payload := adminSessionPayload{
		Sub: username,
		Exp: time.Now().Add(adminSessionTTL).Unix(),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal admin session payload: %w", err)
	}

	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sig := signAdminPayload(secret, encodedPayload)
	return encodedPayload + "." + sig, nil
}

func verifyAdminSessionToken(token string) (string, error) {
	secret := adminSessionSecret()
	if secret == "" {
		return "", errors.New("admin session secret is not configured")
	}

	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", errors.New("malformed admin session token")
	}

	encodedPayload, sig := parts[0], parts[1]
	expected := signAdminPayload(secret, encodedPayload)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return "", errors.New("invalid admin session token signature")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return "", errors.New("malformed admin session payload")
	}

	var payload adminSessionPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return "", errors.New("malformed admin session payload json")
	}

	if time.Now().Unix() > payload.Exp {
		return "", errors.New("admin session token expired")
	}
	if payload.Sub == "" {
		return "", errors.New("missing subject in admin session token")
	}

	return payload.Sub, nil
}

func signAdminPayload(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ── Cookie helpers ────────────────────────────────────────────────────────────

func setAdminSessionCookie(c *gin.Context, token string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(adminSessionCookie, url.QueryEscape(token), int(adminSessionTTL.Seconds()), "/", "", authCookieSecure(), true)
}

func clearAdminSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(adminSessionCookie, "", -1, "/", "", authCookieSecure(), true)
}

func setCameraAuthCookie(c *gin.Context, token string, claims *nextauth.TokenClaims) {
	maxAge := 12 * time.Hour
	if claims != nil && claims.ExpiresAt > 0 {
		if untilExpiry := time.Until(time.Unix(claims.ExpiresAt, 0)); untilExpiry > 0 && untilExpiry < maxAge {
			maxAge = untilExpiry
		}
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cameraAccessTokenCookie, url.QueryEscape(token), int(maxAge.Seconds()), "/", "", authCookieSecure(), true)
}

func clearCameraAuthCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cameraAccessTokenCookie, "", -1, "/", "", authCookieSecure(), true)
}

// ── cameraAuthUser helpers ────────────────────────────────────────────────────

// DisplayName returns a human-readable name safe to use in templates.
// Super admins show their username; NextAuth users show their token name.
func (u *cameraAuthUser) DisplayName() string {
	if u == nil {
		return ""
	}
	if u.IsSuperAdmin {
		return u.AdminUser
	}
	if u.Claims != nil {
		return u.Claims.Name
	}
	return ""
}

// DisplayEmail returns the user's email safe to use in templates.
func (u *cameraAuthUser) DisplayEmail() string {
	if u == nil {
		return ""
	}
	if u.Claims != nil {
		return u.Claims.Email
	}
	return ""
}

// IsAdminUser returns true for super admins and for NextAuth users with isAdmin=true.
func (u *cameraAuthUser) IsAdminUser() bool {
	if u == nil {
		return false
	}
	if u.IsSuperAdmin {
		return true
	}
	return u.Claims != nil && u.Claims.IsAdmin
}

// ── Context helpers ───────────────────────────────────────────────────────────

func getCameraAuthUser(c *gin.Context) (*cameraAuthUser, bool) {
	value, ok := c.Get(authContextUserKey)
	if !ok {
		return nil, false
	}
	user, ok := value.(*cameraAuthUser)
	return user, ok
}

// requireCameraAdmin allows super admins (username/password login) or users
// whose NextAuth token carries isAdmin=true.
func requireCameraAdmin(c *gin.Context) (*cameraAuthUser, bool) {
	user, ok := getCameraAuthUser(c)
	if !ok {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin camera access is required"})
		return nil, false
	}
	if user.IsSuperAdmin {
		return user, true
	}
	if user.Claims == nil || !user.Claims.IsAdmin {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin camera access is required"})
		return nil, false
	}
	return user, true
}

// ── Middleware ────────────────────────────────────────────────────────────────

// authMiddleware is used for browser UI routes. On failure it redirects to /auth/login.
func authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, failure := authenticateCameraRequest(c)
		if failure != nil {
			reason := "unauthorized"
			if failure.Status == http.StatusForbidden {
				reason = "forbidden"
			}
			redirect := c.Request.URL.RequestURI()
			target := "/auth/login?reason=" + reason + "&redirect=" + url.QueryEscape(redirect)
			c.Redirect(http.StatusFound, target)
			c.Abort()
			return
		}
		c.Set(authContextUserKey, user)
		c.Next()
	}
}

// apiAuthMiddleware is used for /api/* routes. On failure it returns JSON.
func apiAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, failure := authenticateCameraRequest(c)
		if failure != nil {
			c.AbortWithStatusJSON(failure.Status, gin.H{"error": failure.Message})
			return
		}
		c.Set(authContextUserKey, user)
		c.Next()
	}
}

// ── Core auth logic ───────────────────────────────────────────────────────────

// authenticateCameraRequest tries the super admin session first, then falls
// through to the NextAuth token + allowlist path.
func authenticateCameraRequest(c *gin.Context) (*cameraAuthUser, *authFailure) {
	if user, ok := authenticateSuperAdmin(c); ok {
		return user, nil
	}
	return authenticateExternalRequest(c)
}

// authenticateSuperAdmin checks for a valid camera_admin_session cookie
// (set after a successful username/password login).
func authenticateSuperAdmin(c *gin.Context) (*cameraAuthUser, bool) {
	raw, err := c.Cookie(adminSessionCookie)
	if err != nil || raw == "" {
		return nil, false
	}

	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		decoded = raw
	}

	username, err := verifyAdminSessionToken(decoded)
	if err != nil {
		log.Printf("camera admin session invalid: %v", err)
		return nil, false
	}

	return &cameraAuthUser{
		IsSuperAdmin: true,
		AdminUser:    username,
	}, true
}

// authenticateExternalRequest handles requests from external callers (e.g.
// porteight-suvidhi). It decodes the NextAuth JWE token and checks the
// camera_access_users allowlist.
func authenticateExternalRequest(c *gin.Context) (*cameraAuthUser, *authFailure) {
	secret := nextAuthSecret()
	if secret == "" {
		log.Printf("camera auth denied: NEXTAUTH_SECRET is missing")
		return nil, &authFailure{Status: http.StatusInternalServerError, Message: "camera auth is not configured"}
	}

	token := extractExternalAuthToken(c)
	if token == "" {
		log.Printf("camera auth denied: missing auth token path=%s", c.Request.URL.Path)
		return nil, &authFailure{Status: http.StatusUnauthorized, Message: "missing auth token"}
	}

	claims, err := nextauth.DecodeNextAuthToken(secret, token)
	if err != nil {
		log.Printf("camera auth denied: invalid token path=%s err=%v", c.Request.URL.Path, err)
		return nil, &authFailure{Status: http.StatusUnauthorized, Message: "invalid or expired auth token"}
	}

	allowEntry, err := db.GetCameraAccessUser(claims.UserID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("camera auth denied: user %s not allowlisted path=%s", claims.UserID, c.Request.URL.Path)
			return nil, &authFailure{Status: http.StatusForbidden, Message: "camera access is not enabled for this user"}
		}
		log.Printf("camera auth lookup failed for user %s: %v", claims.UserID, err)
		return nil, &authFailure{Status: http.StatusInternalServerError, Message: "failed to validate camera access"}
	}
	if !allowEntry.Enabled {
		log.Printf("camera auth denied: user %s allowlist disabled path=%s", claims.UserID, c.Request.URL.Path)
		return nil, &authFailure{Status: http.StatusForbidden, Message: "camera access is disabled for this user"}
	}

	if allowEntry.Email == "" && claims.Email != "" {
		allowEntry.Email = claims.Email
	}
	if allowEntry.Name == "" && claims.Name != "" {
		allowEntry.Name = claims.Name
	}
	if err := db.UpsertCameraAccessUser(allowEntry); err != nil {
		log.Printf("camera auth user refresh failed for user %s: %v", claims.UserID, err)
	}
	if err := db.TouchCameraAccessUserValidation(claims.UserID); err != nil {
		log.Printf("camera auth touch failed for user %s: %v", claims.UserID, err)
	}

	return &cameraAuthUser{
		Token:      token,
		Claims:     claims,
		AllowEntry: allowEntry,
	}, nil
}

// extractExternalAuthToken reads the NextAuth token from:
//  1. Authorization: Bearer <token> header
//  2. camera_access_token cookie (set by the old bootstrap flow, kept for compat)
//  3. next-auth.session-token or __Secure-next-auth.session-token cookie
func extractExternalAuthToken(c *gin.Context) string {
	authHeader := c.GetHeader("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	}

	for _, cookieName := range []string{
		cameraAccessTokenCookie,
		"next-auth.session-token",
		"__Secure-next-auth.session-token",
	} {
		if value, err := c.Cookie(cookieName); err == nil && value != "" {
			if decoded, err := url.QueryUnescape(value); err == nil {
				return decoded
			}
			return value
		}
	}
	return ""
}
