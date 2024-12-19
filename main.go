package main

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/routes"
	"CuTePi/ctp"
)

func main() {
  defer ctp.CloseDB();
	// Load configuration
	config.LoadConfig()

	r := gin.Default()
	r.SetTrustedProxies(nil)

	extendedFuncs := map[string]any{
		"contains":  strings.Contains,
		"hasPrefix": strings.HasPrefix,
		"hasSuffix": strings.HasSuffix,
	}

	// Load templates with FunctionMap
	r.SetHTMLTemplate(template.Must(template.New("").Funcs(extendedFuncs).ParseGlob("templates/*")))

	index := r.Group("/")
	api := r.Group("/api")
	install := r.Group("/install")
	upload := r.Group("/upload")
	youtube := r.Group("/youtube")

	{
		routes.Public(r)
		routes.Index(index)
		routes.Api(api)
		routes.Install(install)
		routes.Upload(upload)
		routes.Youtube(youtube)
	}

	// Start the server
	address := fmt.Sprintf(":%d", config.Port())
	printNetworkInfo()
	r.Run(address)

}
