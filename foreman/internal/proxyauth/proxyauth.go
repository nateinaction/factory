// Package proxyauth holds the run-token check and path check both of foreman's loopback proxies use.
// One implementation keeps the constant-time comparison and the API-prefix rule the same everywhere.
// The address check, transport and serve loop those proxies share live in internal/proxy.
package proxyauth

import (
	"crypto/subtle"
	"net/http"
	"path"
	"strings"
)

// Authorized reports whether r carries token in its Authorization header. The comparison is
// constant time, and the token never reaches a log.
func Authorized(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// UnderPrefix reports whether p is prefix or a path beneath it.
func UnderPrefix(p, prefix string) bool {
	clean := path.Clean(p)

	return clean == prefix || strings.HasPrefix(clean, prefix+"/")
}
