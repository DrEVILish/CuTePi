package routes

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

// imageExts: the only client-cacheable data is images.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".svg": true, ".avif": true, ".ico": true,
}

// CachePolicy: everything is served with no-store so the client never
// caches stale UI or data — except images (pool thumbnails, logos,
// placeholders), which get a short private cache. Registered globally so
// HTML, CSS, JS, API and media responses all follow it.
//
// Pool files under /media are never cached: a replace-upload keeps the
// name, so a cached copy would show the old picture. Thumbnails carry a
// ?v= stamp (mediapoolView) that changes when they are regenerated.
// "private" keeps a reverse proxy from caching responses that may sit
// behind the operator password.
func CachePolicy() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		cacheable := func(path string) bool {
			if strings.HasPrefix(path, "/media/") {
				return false
			}
			dot := strings.LastIndexByte(path, '.')
			if dot < 0 {
				return false
			}
			return imageExts[strings.ToLower(path[dot:])]
		}
		if cacheable(p) {
			c.Header("Cache-Control", "private, max-age=3600")
		} else {
			c.Header("Cache-Control", "no-store")
		}
		c.Next()
	}
}

func Public(r *gin.Engine) {
	r.StaticFS("/css", filesOnly("./public/css"))
	// ftl-themes bundles and their fonts, served as siblings: a bundle at
	// /ftl/themes/x.css resolves its ../assets/fonts/... to /ftl/assets/fonts/,
	// which is the arrangement that library's CONTRACT.md requires.
	r.StaticFS("/ftl/themes", filesOnly("./third_party/ftl-themes/dist"))
	r.StaticFS("/ftl/assets", filesOnly("./third_party/ftl-themes/assets"))
	r.StaticFS("/fonts", filesOnly("./public/fonts"))
	r.StaticFS("/img", filesOnly("./public/img"))
	r.StaticFS("/src", filesOnly("./public/src"))
	r.StaticFS("/thumbnails", filesOnly(config.ThumbnailLocation()))
	r.StaticFS("/media", filesOnly(config.MediaLocation()))
}

// filesOnly serves the files under dir but answers 404 for directories:
// http.Dir alone renders a listing of every folder (the whole media pool,
// staging leftovers) to anyone who asks for /media/.
func filesOnly(dir string) http.FileSystem {
	return noDirFS{http.Dir(dir)}
}

type noDirFS struct{ fs http.FileSystem }

func (n noDirFS) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || st.IsDir() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}
