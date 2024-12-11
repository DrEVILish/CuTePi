package routes

import (
	"net/http"
	"github.com/gin-gonic/gin"
)

type Mediapool struct{
  Filename string
  Size string
  Mimetype string
  Thumbnail string
}

func Index(rg *gin.RouterGroup){
  rg.GET("/", func(c *gin.Context){

    mediapool := []Mediapool{
        {Filename: "File1", Size: "12MB", Mimetype: "video/mp4", Thumbnail: "yes"},
        {Filename: "File2", Size: "1MB", Mimetype: "image/jpg"},
        {Filename: "File3", Size: "2GB", Mimetype: "video/mp4"},
    }

    c.HTML(http.StatusOK, "index.html", gin.H{
      "Mediapool": mediapool,
      "title": "woo",

    })
  })
}
