package routes

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"CuTePi/gsp"
	"CuTePi/ctp"
)

func Api(rg *gin.RouterGroup) {
	rg.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "CuTePi API avaliable")
	})
	rg.POST("/play", func(c *gin.Context) {
		fmt.Println("PLAY")

		gsp.Play()

		c.Status(http.StatusOK)
	})
	rg.POST("/pause", func(c *gin.Context) {
		fmt.Println("PAUSE")
		gsp.Pause()
		c.Status(http.StatusOK)
	})
	rg.POST("/togglePause", func(c *gin.Context) {
		fmt.Println("togglePause")
		gsp.TogglePause()
		// if(!gsp.isPaused){
		//   res.send("resume");
		// }else {
		//   res.send("pause");
		// }
		c.Status(http.StatusOK)
	})
	rg.POST("/fadeOut", func(c *gin.Context) {
		fmt.Println("fadeOut")
		gsp.FadeOut()
		c.Status(http.StatusOK)
	})
	rg.POST("/panic", func(c *gin.Context) {
		fmt.Println("!!PANIC!!")
		gsp.Panic()
		c.Status(http.StatusOK)
	})
	rg.POST("/clear", func(c *gin.Context) {
		fmt.Println("Clear CueSheet")
		err := ctp.ClearCueSheet()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		cuesheet := ctp.GetCuesheet()
		c.HTML(http.StatusOK, "cuesheet.html", gin.H{
			"cuesheet": cuesheet,
		})
	})
	rg.POST("/next", func(c *gin.Context) {
		fmt.Println("NEXT")
		gsp.Next()
		c.Status(http.StatusOK)
	})
	rg.POST("/prev", func(c *gin.Context) {
		fmt.Println("PREV")
		gsp.Prev()
		c.Status(http.StatusOK)
	})
	rg.POST("/stop", func(c *gin.Context) {
		fmt.Println("STOP")
		gsp.Stop()
		c.Status(http.StatusOK)
	})

	rg.POST("/test/*pattern", func(c *gin.Context) {
		pattern := c.Param("pattern")
		if pattern != "" {
			gsp.ShowTest(pattern)
		} else {
			gsp.ShowTest("smpte-rp-219")
		}
		fmt.Println("Show Default Test")
		c.Status(http.StatusOK)
	})

	// Direct Play from Mediapool
	rg.POST("/play/:filename", func(c *gin.Context) {
		filename := c.Param("filename")
		fmt.Println("Direct Play" + filename)
		gsp.Load(filename)
		gsp.Play()
		c.Status(http.StatusOK)
	})

	rg.POST("/cue/add/:filename/*cuePos", func(c *gin.Context) {
		filename := c.Param("filename")
		cuePos := c.Param("cuePos")

		if cuePos != "" {
			fmt.Println("Add at" + cuePos + "new Cue" + filename)
		} else {
			fmt.Println("Add new Cue" + filename)
		}
		err := ctp.AddCue(filename, cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		cuesheet := ctp.GetCuesheet()
		c.HTML(http.StatusOK, "cuesheet.html", gin.H{
			"cuesheet": cuesheet,
		})
	})

	rg.DELETE("/media/:filename", func(c *gin.Context) {
		filename := c.Param("filename")
		fmt.Println("Delete" + filename)

		if gsp.CurrentPlaying() == filename {
			fmt.Println("Can't delete currently playing file")
			c.Status(http.StatusConflict)
			return
		}
		err := ctp.Delete(filename)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.Status(http.StatusOK)
	})

	rg.POST("/cue/next", func(c *gin.Context) {
		fmt.Println("Select Next Cue (down)")
		err := ctp.NextCue()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		cuesheet := ctp.GetCuesheet()
		c.HTML(http.StatusOK, "cuesheet.html", gin.H{
			"cuesheet": cuesheet,
		})
	})
	rg.POST("/cue/prev", func(c *gin.Context) {
		fmt.Println("Select Prev Cue (up)")
		err := ctp.PrevCue()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		cuesheet := ctp.GetCuesheet()
		c.HTML(http.StatusOK, "cuesheet.html", gin.H{
			"cuesheet": cuesheet,
		})
		c.Status(http.StatusOK)
	})

	rg.POST("/cue/:cuePos", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		fmt.Println("Selected" + cuePos)
		err := ctp.SetCue(cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		cuesheet := ctp.GetCuesheet()
		c.HTML(http.StatusOK, "cuesheet.html", gin.H{
			"cuesheet": cuesheet,
		})
	})

	rg.POST("/cue/:cuePos/edit/:col", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		col := c.Param("col")
		fmt.Println("Edit" + col + "of CueNo" + cuePos)
		val,err := ctp.GetCue(cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.HTML(http.StatusOK, "cueeditcol.html", gin.H{
			"cuePos": cuePos,
			"col":    col,
			"val":    val,
		})
	})
	rg.PUT("/cue/:cuePos/edit/:col", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		col := c.Param("col")
		val := c.PostForm("val")
		fmt.Println("Update", cuePos, "Column", col, "Value", val)
		err := ctp.UpdateCue(cuePos, col, val)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		cuesheet := ctp.GetCuesheet()
		c.HTML(http.StatusOK, "cuesheet.html", gin.H{
			"cuesheet": cuesheet,
		})
	})
	rg.DELETE("/cue/:cuePos", func(c *gin.Context) {
		cuePos := c.Param("cuePos")
		fmt.Println("Remove Cue" + cuePos)
		err := ctp.RemoveCue(cuePos)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": err.Error(),
			})
			return
		}
		c.Status(http.StatusOK)
	})

}
