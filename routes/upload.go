package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Upload(rg *gin.RouterGroup){
  rg.GET("/", func(c *gin.Context){
    c.String(http.StatusOK, "upload pong")
  })
}
