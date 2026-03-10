package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

const (
	// NextAuth.js v4 info strings ("NextAuth.js" prefix)
	nextAuthV4Info = "NextAuth.js Generated Encryption Key"

	// Auth.js v5 info strings ("Auth.js" prefix)
	authJSInfo = "Auth.js Generated Encryption Key"

	// Cookie name salts
	defaultNextAuthSalt      = "next-auth.session-token"
	secureNextAuthSalt       = "__Secure-next-auth.session-token"
	defaultAuthJSSessionSalt = "authjs.session-token"
	secureAuthJSSessionSalt  = "__Secure-authjs.session-token"
)

var (
	ErrMissingSecret       = errors.New("missing NEXTAUTH_SECRET")
	ErrInvalidToken        = errors.New("invalid nextauth token")
	ErrExpiredToken        = errors.New("expired nextauth token")
	ErrMissingUserID       = errors.New("missing user id in token")
	ErrUnsupportedEncoding = errors.New("unsupported nextauth token encoding")
)

type TokenClaims struct {
	Subject   string         `json:"sub"`
	UserID    string         `json:"id"`
	Name      string         `json:"name"`
	Email     string         `json:"email"`
	Picture   string         `json:"picture"`
	TBToken   string         `json:"tbToken"`
	IsAdmin   bool           `json:"isAdmin"`
	IssuedAt  int64          `json:"iat"`
	ExpiresAt int64          `json:"exp"`
	JTI       string         `json:"jti"`
	Raw       map[string]any `json:"-"`
}

func (c *TokenClaims) Normalize() error {
	if c.UserID == "" {
		c.UserID = c.Subject
	}
	if c.UserID == "" {
		return ErrMissingUserID
	}
	if c.ExpiresAt > 0 && time.Now().Unix() > c.ExpiresAt {
		return ErrExpiredToken
	}
	return nil
}

func DecodeNextAuthToken(secret string, token string) (*TokenClaims, error) {
	if secret == "" {
		return nil, ErrMissingSecret
	}
	if token == "" {
		return nil, ErrInvalidToken
	}

	parts := strings.Split(token, ".")
	if len(parts) != 5 {
		return nil, ErrInvalidToken
	}

	protectedHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: malformed protected header", ErrInvalidToken)
	}

	var header struct {
		Alg string `json:"alg"`
		Enc string `json:"enc"`
	}
	if err := json.Unmarshal(protectedHeader, &header); err != nil {
		return nil, fmt.Errorf("%w: malformed header json", ErrInvalidToken)
	}
	if header.Alg != "dir" || header.Enc != "A256GCM" {
		return nil, fmt.Errorf("%w: alg=%s enc=%s", ErrUnsupportedEncoding, header.Alg, header.Enc)
	}
	if parts[1] != "" {
		return nil, fmt.Errorf("%w: unexpected encrypted key", ErrInvalidToken)
	}

	iv, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: malformed iv", ErrInvalidToken)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, fmt.Errorf("%w: malformed ciphertext", ErrInvalidToken)
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, fmt.Errorf("%w: malformed tag", ErrInvalidToken)
	}

	strategies := []struct {
		salt string
		info string
	}{
		// next-auth v4.22+ — salt = cookie name, info includes it in parentheses
		{salt: defaultNextAuthSalt, info: fmt.Sprintf("%s (%s)", nextAuthV4Info, defaultNextAuthSalt)},
		{salt: secureNextAuthSalt, info: fmt.Sprintf("%s (%s)", nextAuthV4Info, secureNextAuthSalt)},
		// next-auth v4 (older, no salt)
		{salt: "", info: nextAuthV4Info},
		// Auth.js v5 — salt = cookie name
		{salt: defaultNextAuthSalt, info: fmt.Sprintf("%s (%s)", authJSInfo, defaultNextAuthSalt)},
		{salt: secureNextAuthSalt, info: fmt.Sprintf("%s (%s)", authJSInfo, secureNextAuthSalt)},
		{salt: defaultAuthJSSessionSalt, info: fmt.Sprintf("%s (%s)", authJSInfo, defaultAuthJSSessionSalt)},
		{salt: secureAuthJSSessionSalt, info: fmt.Sprintf("%s (%s)", authJSInfo, secureAuthJSSessionSalt)},
		// Auth.js v5 (no salt)
		{salt: "", info: authJSInfo},
	}

	aad := []byte(parts[0])
	combined := append(ciphertext, tag...)
	var plaintext []byte
	var decryptErr error
	for _, strategy := range strategies {
		key, err := deriveEncryptionKey(secret, strategy.salt, strategy.info, header.Enc)
		if err != nil {
			return nil, err
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("create aes cipher: %w", err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create gcm cipher: %w", err)
		}
		if len(iv) != gcm.NonceSize() {
			return nil, fmt.Errorf("%w: invalid nonce size", ErrInvalidToken)
		}
		plaintext, err = gcm.Open(nil, iv, combined, aad)
		if err == nil {
			decryptErr = nil
			break
		}
		decryptErr = err
	}
	if decryptErr != nil {
		return nil, fmt.Errorf("%w: decrypt failed", ErrInvalidToken)
	}

	var raw map[string]any
	if err := json.Unmarshal(plaintext, &raw); err != nil {
		return nil, fmt.Errorf("%w: malformed payload json", ErrInvalidToken)
	}

	claims := &TokenClaims{
		Subject:   getString(raw["sub"]),
		UserID:    getString(raw["id"]),
		Name:      getString(raw["name"]),
		Email:     getString(raw["email"]),
		Picture:   getString(raw["picture"]),
		TBToken:   getString(raw["tbToken"]),
		IsAdmin:   getBool(raw["isAdmin"]),
		IssuedAt:  getInt64(raw["iat"]),
		ExpiresAt: getInt64(raw["exp"]),
		JTI:       getString(raw["jti"]),
		Raw:       raw,
	}
	if err := claims.Normalize(); err != nil {
		return nil, err
	}
	return claims, nil
}

func deriveEncryptionKey(secret string, salt string, info string, enc string) ([]byte, error) {
	length := 32
	switch enc {
	case "A256GCM":
		length = 32
	case "A256CBC-HS512":
		length = 64
	default:
		return nil, fmt.Errorf("%w: enc=%s", ErrUnsupportedEncoding, enc)
	}

	reader := hkdf.New(sha256.New, []byte(secret), []byte(salt), []byte(info))
	key := make([]byte, length)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("derive nextauth key: %w", err)
	}
	return key, nil
}

func getString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	default:
		return ""
	}
}

func getBool(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(value, "true")
	default:
		return false
	}
}

func getInt64(v any) int64 {
	switch value := v.(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	case json.Number:
		n, _ := value.Int64()
		return n
	case string:
		var n json.Number = json.Number(value)
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}
