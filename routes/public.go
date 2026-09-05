package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
)

func Public(r *gin.Engine) {
	r.StaticFS("/css", http.Dir("./public/css"))
	r.StaticFS("/fonts", http.Dir("./public/fonts"))
	r.StaticFS("/icons", http.Dir("./public/icons"))
	r.StaticFS("/img", http.Dir("./public/img"))
	r.StaticFS("/src", http.Dir("./public/src"))
	r.StaticFS("/thumbnails", http.Dir(config.ThumbnailLocation()))
	r.StaticFS("/media", http.Dir(config.MediaLocation()))
}
