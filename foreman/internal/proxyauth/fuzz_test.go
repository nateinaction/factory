package proxyauth

import (
	"path"
	"strings"
	"testing"
)

// FuzzUnderPrefix checks that a path UnderPrefix accepts never climbs out of the prefix.
func FuzzUnderPrefix(f *testing.F) {
	for _, seed := range []string{
		"/v1", "/v1/chat", "/v1/../etc", "/v1/./x", "/v1//x", "/v1x", "/v1/..", "//v1", "v1/x", "/v1/%2e%2e/x", "",
	} {
		f.Add(seed)
	}

	const prefix = "/v1"

	f.Fuzz(func(t *testing.T, p string) {
		if !UnderPrefix(p, prefix) {
			return
		}

		for segment := range strings.SplitSeq(path.Clean(p), "/") {
			if segment == ".." {
				t.Fatalf("UnderPrefix(%q, %q) accepted a path that climbs out", p, prefix)
			}
		}
	})
}
