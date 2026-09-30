package routes

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
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
	// Before sending any bytes, the browser asks which names would clash so
	// the operator chooses replace / keep both / skip up front (a phone
	// video is not uploaded twice).
	rg.POST("/check", func(c *gin.Context) {
		var body struct {
			Names []string `json:"names"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.String(http.StatusBadRequest, "invalid check payload")
			return
		}
		c.JSON(http.StatusOK, gin.H{"conflicts": uploadConflicts(body.Names)})
	})
}

// Upload name-conflict choices (form field onConflict).
const (
	conflictReplace = "replace" // overwrite the pool file of the same name
	conflictRename  = "rename"  // keep both: the upload becomes "name (2).ext"
	conflictSkip    = "skip"    // leave the pool file, drop the upload
)

// uploadConflicts lists the names that already exist in the media folder or
// repeat within the same batch.
func uploadConflicts(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		n = filepath.Base(n)
		if seen[n] {
			out = append(out, n)
			continue
		}
		seen[n] = true
		if mediaFileExists(n) {
			out = append(out, n)
		}
	}
	return out
}

func mediaFileExists(name string) bool {
	_, err := os.Lstat(filepath.Join(config.MediaLocation(), name))
	return err == nil
}

// freeMediaName returns "name (2).ext", "name (3).ext", … — the first name
// not yet in the media folder.
func freeMediaName(name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !mediaFileExists(cand) {
			return cand
		}
	}
}

func handleUpload(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid upload: "+err.Error())
		return
	}

	files := form.File["media"]
	if len(files) == 0 {
		respondError(c, http.StatusBadRequest, "no files uploaded")
		return
	}

	onConflict := c.PostForm("onConflict")
	switch onConflict {
	case "":
		names := make([]string, len(files))
		for i, fh := range files {
			names[i] = fh.Filename
		}
		if conflicts := uploadConflicts(names); len(conflicts) > 0 {
			c.JSON(http.StatusConflict, gin.H{
				"conflicts": conflicts,
				"error":     "already in the media pool: " + strings.Join(conflicts, ", ") + " (choose replace, rename or skip)",
			})
			return
		}
	case conflictReplace, conflictRename, conflictSkip:
	default:
		respondError(c, http.StatusBadRequest, "onConflict must be replace, rename or skip")
		return
	}

	// Best-effort multi-file: every file that validates is imported, and ALL
	// failures are reported together instead of aborting the batch on the
	// first error (which silently dropped every later file while keeping the
	// earlier ones - a half-import the operator couldn't see).
	var failures []string
	var imported, skipped []string
	seen := map[string]bool{}
	for _, fh := range files {
		name := filepath.Base(fh.Filename)
		if seen[name] || mediaFileExists(name) {
			switch onConflict {
			case conflictSkip:
				skipped = append(skipped, name)
				continue
			case conflictRename:
				name = freeMediaName(name)
			}
		}
		seen[name] = true
		if err := saveUploadedFile(fh, name); err != nil {
			failures = append(failures, fmt.Sprintf("%q: %v", fh.Filename, err))
			continue
		}
		imported = append(imported, name)
	}
	// Percent-encoded: header values are Latin-1 to XHR, file names are not.
	if summary, err := json.Marshal(gin.H{"imported": imported, "skipped": skipped}); err == nil {
		c.Header("X-Upload-Result", url.PathEscape(string(summary)))
	}
	if len(failures) > 0 {
		respondError(c, http.StatusUnprocessableEntity, fmt.Sprintf("could not import %d of %d: %s", len(failures), len(files), strings.Join(failures, "; ")))
		return
	}

	// htmx requests (the desktop modal) get the mediapool partial to swap
	// into #mediapool in place; plain form posts (the standalone page) do a
	// full navigation, so send them back to the control centre.
	if c.GetHeader("HX-Request") != "" {
		mediapool, err := mediapoolView()
		if err != nil {
			respondError(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.HTML(http.StatusOK, "mediapool.html", gin.H{"Mediapool": mediapool})
		return
	}
	c.Redirect(http.StatusSeeOther, "/")
}

// saveUploadedFile validates, saves to the media directory as filename, probes
// metadata, and registers fh in the mediapool. On any failure the partially
// written file is removed and the import is rejected, per spec (a media
// file with unextractable duration/resolution/codec is not imported).
func saveUploadedFile(fh *multipart.FileHeader, filename string) error {
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		return fmt.Errorf("invalid filename")
	}
	if media.KindFromExtension(filename) == media.KindUnknown {
		return fmt.Errorf("unsupported media type")
	}

	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	// Stage in a unique temp file under the data dir's tmp/ and only move it
	// into place once it has passed probe/verify/registration (importMedia).
	// Unique per upload: two concurrent uploads of the same name used to
	// share one "name.uploading" sidecar and interleave into a corrupt file.
	// Outside the media dir, so a half-written upload is never served at
	// /media or seen as a pool file.
	if err := os.MkdirAll(config.TmpDir(), 0o755); err != nil {
		return err
	}
	dst, err := os.CreateTemp(config.TmpDir(), "upload-*"+filepath.Ext(filename))
	if err != nil {
		return err
	}
	tmpPath := dst.Name()
	if _, err = io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		return err
	}
	// A failed close can mean the data never reached disk (full disk, I/O
	// error): never import a file that may be truncated.
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	return importMedia(filename, tmpPath)
}
