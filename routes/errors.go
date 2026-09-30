package routes

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

// isHTMX reports whether the request was issued by htmx (HX-Request set).
func isHTMX(c *gin.Context) bool {
	return c.GetHeader("HX-Request") != ""
}

// respondError is the single way handlers report a failure to the client.
// htmx requests get the bare message as text/plain with the same status, so
// the client can show it as-is (a full error page swapped into a fragment
// was the old O4/D12 bug); normal browser navigations still get the full
// error.html page. Server paths are redacted from the message either way.
func respondError(c *gin.Context, status int, msg string) {
	msg = redactPaths(msg)
	if isHTMX(c) {
		c.String(status, "%s", msg)
		return
	}
	c.HTML(status, "error.html", gin.H{"error": msg})
}

// redactPaths strips the server's data, temp, media and thumbnail directory
// prefixes from a user-facing message, so errors name files, never where the
// server keeps them (O5). Both the configured and the absolute spelling of
// each directory are removed, longest first so a nested directory (tmp/
// inside the working dir) is stripped whole.
func redactPaths(msg string) string {
	sep := string(filepath.Separator)
	var rel, abs []string
	for _, d := range []string{config.TmpDir(), config.MediaLocation(), config.ThumbnailLocation(), filepath.Dir(config.DbLocation()), config.WorkingDir()} {
		if d == "" {
			continue
		}
		if a, err := filepath.Abs(d); err == nil && a != sep {
			abs = append(abs, a)
		}
		if c := filepath.Clean(d); !filepath.IsAbs(c) && c != "." {
			rel = append(rel, c)
		}
	}
	byLen := func(s []string) {
		sort.Slice(s, func(i, j int) bool { return len(s[i]) > len(s[j]) })
	}
	byLen(abs)
	byLen(rel)
	for _, d := range abs {
		msg = strings.ReplaceAll(msg, d+sep, "")
	}
	for _, d := range abs {
		msg = strings.ReplaceAll(msg, d, "")
	}
	// A relative spelling is only stripped as a path prefix ("media/x"),
	// never as a bare word: "media" also opens every media error message.
	for _, d := range rel {
		msg = strings.ReplaceAll(msg, d+sep, "")
	}
	return msg
}

// importError rewrites an import failure for the user: the staging path
// (and its temp base name) become the user's file name, and any remaining
// server directory is stripped.
func importError(err error, filename, srcPath string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if srcPath != "" {
		msg = strings.ReplaceAll(msg, srcPath, filename)
		if base := filepath.Base(srcPath); base != filename {
			msg = strings.ReplaceAll(msg, base, filename)
		}
	}
	return &userError{msg: redactPaths(msg), err: err}
}

// userError carries a path-free message while keeping the original error
// for errors.Is/As.
type userError struct {
	msg string
	err error
}

func (e *userError) Error() string { return e.msg }
func (e *userError) Unwrap() error { return e.err }
