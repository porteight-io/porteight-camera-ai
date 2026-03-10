package main

import (
	"fmt"
	"net/http"
	"time"
)

const mediaURLTTL = 12 * time.Hour

func signedRelativePath(path string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	sig := signPath(http.MethodGet, path, exp)
	return fmt.Sprintf("%s?exp=%d&sig=%s", path, exp, sig)
}
