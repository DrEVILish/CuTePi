package routes

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/media"
)

// Show compiler (DESIGN §12.17): prepare the show's slow videos as HEVC
// copies this Pi decodes in hardware, in the background while the wall is
// idle. POST queues them, GET reports the queue, DELETE cancels it.
var preparer = &media.Preparer{Run: prepareOne}

func prepareOne(ctx context.Context, filename string) error {
	v := ctp.MediaVideoInfo(filename)
	if v == nil || v.Alpha {
		return media.ErrNotPreparable
	}
	w, h, hz := gsp.DisplayMode()
	src := filepath.Join(config.MediaLocation(), filename)
	logs.Printf(logs.RTEUpdate, "prepare: encoding %q", filename)
	if err := media.PrepareVideo(ctx, src, media.PreparedPath(filename), v, w, h, hz); err != nil {
		logs.PrintfWarn(logs.RTEUpdate, "prepare: %q: %v", filename, err)
		return err
	}
	logs.Printf(logs.RTEUpdate, "prepare: %q ready", filename)
	return nil
}

// prepareCandidates are the sheet's video files that would play below full
// rate here (all = every video on the sheet), not yet prepared.
func prepareCandidates(all bool) ([]string, error) {
	sheet, err := ctp.GetCuesheet()
	if err != nil {
		return nil, err
	}
	_, _, hz := gsp.DisplayMode()
	seen := map[string]bool{}
	var out []string
	for _, cue := range sheet.Cues {
		f := cue.Filename
		if f == "" || seen[f] || !strings.HasPrefix(cue.Mimetype, "video/") {
			continue
		}
		seen[f] = true
		if media.Prepared(config.MediaLocation(), f) != "" {
			continue
		}
		v := ctp.MediaVideoInfo(f)
		if v == nil || v.Alpha {
			continue
		}
		if all || media.PlaybackWarning(v, hz) != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

func registerPrepareRoutes(rg *gin.RouterGroup) {
	rg.GET("/show/prepare", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"jobs": preparer.Jobs()})
	})
	rg.POST("/show/prepare", func(c *gin.Context) {
		files, err := prepareCandidates(c.PostForm("all") == "1")
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		preparer.Enqueue(files)
		c.JSON(http.StatusOK, gin.H{"queued": files, "jobs": preparer.Jobs()})
	})
	rg.DELETE("/show/prepare", func(c *gin.Context) {
		preparer.Cancel()
		c.JSON(http.StatusOK, gin.H{"jobs": preparer.Jobs()})
	})
}
