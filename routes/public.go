package routes

import (
	"net/http"
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
// placeholders), which get a short public cache. Registered globally so
// HTML, CSS, JS, API and media responses all follow it.
func CachePolicy() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		cacheable := func(path string) bool {
			dot := strings.LastIndexByte(path, '.')
			if dot < 0 {
				return false
			}
			return imageExts[strings.ToLower(path[dot:])]
		}
		if cacheable(p) {
			c.Header("Cache-Control", "public, max-age=3600")
		} else {
			c.Header("Cache-Control", "no-store")
		}
		c.Next()
	}
}

func Public(r *gin.Engine) {
	r.StaticFS("/css", http.Dir("./public/css"))
	// ftl-themes bundles and their fonts, served as siblings: a bundle at
	// /ftl/themes/x.css resolves its ../assets/fonts/... to /ftl/assets/fonts/,
	// which is the arrangement that library's CONTRACT.md requires.
	r.StaticFS("/ftl/themes", http.Dir("./third_party/ftl-themes/dist"))
	r.StaticFS("/ftl/assets", http.Dir("./third_party/ftl-themes/assets"))
	r.StaticFS("/fonts", http.Dir("./public/fonts"))
	r.StaticFS("/img", http.Dir("./public/img"))
	r.StaticFS("/src", http.Dir("./public/src"))
	r.StaticFS("/thumbnails", http.Dir(config.ThumbnailLocation()))
	r.StaticFS("/media", http.Dir(config.MediaLocation()))
}
