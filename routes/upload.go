package routes

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

func Upload(rg *gin.RouterGroup) {
	// Standalone mobile-friendly upload page (spec: upload available both as
	// a modal on the desktop control centre and as a separate page).
	rg.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "upload.html", gin.H{
			"title": "Upload",
		})
	})

	// Handles both the modal (an htmx request, targets #mediapool) and the
	// standalone /upload page (a plain full-page form post, redirected back
	// to the control centre on success).
	rg.POST("", handleUpload)
	rg.POST("/", handleUpload)
}

func handleUpload(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "invalid upload: " + err.Error()})
		return
	}

	files := form.File["media"]
	if len(files) == 0 {
		c.HTML(http.StatusBadRequest, "error.html", gin.H{"error": "no files uploaded"})
		return
	}

	for _, fh := range files {
		if err := saveUploadedFile(fh); err != nil {
			c.HTML(http.StatusUnprocessableEntity, "error.html", gin.H{
				"error": fmt.Sprintf("failed to import %q: %v", fh.Filename, err),
			})
			return
		}
	}

	// htmx requests (the desktop modal) get the mediapool partial to swap
	// into #mediapool in place; plain form posts (the standalone page) do a
	// full navigation, so send them back to the control centre.
	if c.GetHeader("HX-Request") != "" {
		mediapool, err := mediapoolView()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
			return
		}
		c.HTML(http.StatusOK, "mediapool.html", gin.H{"Mediapool": mediapool})
		return
	}
	c.Redirect(http.StatusSeeOther, "/")
}

// saveUploadedFile validates, saves to the media directory, probes
// metadata, and registers fh in the mediapool. On any failure the partially
// written file is removed and the import is rejected, per spec (a media
// file with unextractable duration/resolution/codec is not imported).
func saveUploadedFile(fh *multipart.FileHeader) error {
	filename := filepath.Base(fh.Filename)
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		return fmt.Errorf("invalid filename")
	}
	if media.KindFromExtension(filename) == media.KindUnknown {
		return fmt.Errorf("unsupported media type")
	}

	destPath := filepath.Join(config.MediaLocation(), filename)

	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		return err
	}
	size, err := io.Copy(dst, src)
	dst.Close()
	if err != nil {
		os.Remove(destPath)
		return err
	}

	meta, err := media.Probe(destPath)
	if err != nil {
		os.Remove(destPath)
		return err
	}

	title := strings.TrimSuffix(filename, filepath.Ext(filename))
	if err := ctp.RegisterMedia(filename, size, meta, title); err != nil {
		os.Remove(destPath)
		return err
	}
	return nil
}
