package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Youtube(rg *gin.RouterGroup){
  rg.GET("/", func(c *gin.Context){
    c.String(http.StatusOK, "youtube pong")
  })
}
