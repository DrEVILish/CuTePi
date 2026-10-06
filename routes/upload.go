package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
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

// Media names held by imports in progress. An import claims its final name
// (reserveMediaName) before it starts and releases it when it is done, so
// two concurrent imports can never both decide that "clip (2).mp4" is free
// and have the later one replace the earlier. The check against the media
// folder and the claim happen under one lock.
var (
	mediaNameMu   sync.Mutex
	mediaNameHeld = map[string]bool{}
)

// Name reservation modes.
const (
	reserveNew     = iota // the name must be free (not on disk, not held)
	reserveRename         // the name, or the first free "name (n).ext"
	reserveReplace        // the name, replacing a file on disk; refused while held
)

// errNameTaken: reserveNew found the name on disk or held by another import.
var errNameTaken = errors.New("already in the media pool")

// reserveMediaName claims a media name for one import. release must be
// called when the import has finished (moved into place or failed).
func reserveMediaName(name string, mode int) (string, func(), error) {
	mediaNameMu.Lock()
	defer mediaNameMu.Unlock()
	taken := func(n string) bool { return mediaNameHeld[n] || mediaFileExists(n) }
	switch mode {
	case reserveNew:
		if taken(name) {
			return "", nil, errNameTaken
		}
	case reserveRename:
		if taken(name) {
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			for i := 2; ; i++ {
				if cand := fmt.Sprintf("%s (%d)%s", stem, i, ext); !taken(cand) {
					name = cand
					break
				}
			}
		}
	case reserveReplace:
		if mediaNameHeld[name] {
			return "", nil, fmt.Errorf("%q is being imported by another upload", name)
		}
	}
	mediaNameHeld[name] = true
	return name, func() {
		mediaNameMu.Lock()
		delete(mediaNameHeld, name)
		mediaNameMu.Unlock()
	}, nil
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
	// Each file claims its final name before it is written (see
	// reserveMediaName): a name taken meanwhile by another upload is a
	// conflict like one already on disk. Repeats within the batch see the
	// earlier file on disk by then.
	mode := map[string]int{"": reserveNew, conflictSkip: reserveNew, conflictRename: reserveRename, conflictReplace: reserveReplace}[onConflict]
	var failures []string
	var imported, skipped []string
	for _, fh := range files {
		name, release, err := reserveMediaName(filepath.Base(fh.Filename), mode)
		if errors.Is(err, errNameTaken) && onConflict == conflictSkip {
			skipped = append(skipped, filepath.Base(fh.Filename))
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%q: %v", fh.Filename, err))
			continue
		}
		err = saveUploadedFile(fh, name)
		release()
		if err != nil {
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
	// ".." survives the multipart parser's filepath.Base and would make the
	// media dir's parent the import target.
	if !safeMediaName(filename) {
		return fmt.Errorf("invalid filename")
	}
	// No extension allow-list: ffprobe and the decode check below decide
	// (§2, any codec GStreamer can decode plays).

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
	// A part over the multipart memory limit was already spooled to a temp
	// file (TMPDIR is the data dir's tmp/, see main.go): move that file into
	// the staging name instead of writing a multi-GB upload a second time.
	// net/http's MultipartForm.RemoveAll later ignores the vanished spool.
	if f, ok := src.(*os.File); ok {
		if err := os.Rename(f.Name(), tmpPath); err == nil {
			dst.Close()
			return importMedia(filename, tmpPath)
		}
		// Different filesystem (TMPDIR overridden): copy as before.
	}
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
