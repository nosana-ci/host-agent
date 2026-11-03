// Package version holds what the build stamps into the binary.
package version

import (
	"strconv"
	"strings"
)

// Set with -ldflags "-X github.com/nosana-ci/host-agent/internal/version.Version=1.2.3 ...".
var (
	Version = "0.0.0-dev"
	Commit  = ""
	// ReleasePublicKey is the base64 ed25519 key that signs release manifests; empty disables
	// self-update.
	ReleasePublicKey = ""
)

// Compare orders two versions of the form MAJOR.MINOR.PATCH, ignoring a pre-release suffix.
func Compare(a, b string) int {
	pa, pb := parts(a), parts(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parts(v string) [3]int {
	var out [3]int
	v, _, _ = strings.Cut(strings.TrimPrefix(v, "v"), "-")
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}
