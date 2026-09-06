package routes

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/logs"
	"CuTePi/media"
)

// manifestVersion is the format version of cutepi.json inside a .CTP file.
const manifestVersion = 1

// showManifest is cutepi.json: every cue plus the audit trail and the
// exported selection, so import can rebuild order, selection, and history.
type showManifest struct {
	App            string            `json:"app"`
	Version        int               `json:"version"`
	ExportedAt     string            `json:"exportedAt"`
	SelectedCuePos int               `json:"selectedCuePos"`
	Cues           []ctp.ExportCue   `json:"cues"`
	Audit          []logs.AuditEvent `json:"audit"`
}

// Show implements .CTP show export/import (see DESIGN.md). Export zips a
// JSON manifest plus every referenced media file that exists on disk;
// import rebuilds the show from the zip.
func Show(rg *gin.RouterGroup) {
	rg.GET("/show/export", func(c *gin.Context) {
		cues, selected, err := ctp.ExportCues()
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		if len(cues) == 0 {
			c.String(http.StatusBadRequest, "the cuesheet is empty - nothing to export")
			return
		}

		var buf bytes.Buffer
		if err := writeShowZip(&buf, showManifest{
			App:            "CuTePi",
			Version:        manifestVersion,
			ExportedAt:     time.Now().Format(time.RFC3339),
			SelectedCuePos: selected,
			Cues:           cues,
			Audit:          logs.AuditTrail(),
		}); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}

		filename := fmt.Sprintf("show-%s.ctp", time.Now().Format("20060102-150405"))
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		c.Data(http.StatusOK, "application/zip", buf.Bytes())
	})

	rg.POST("/show/import", func(c *gin.Context) {
		mode := strings.TrimSpace(c.PostForm("mode"))
		if mode != "append" && mode != "overwrite" {
			c.String(http.StatusBadRequest, "mode must be append or overwrite")
			return
		}

		fh, err := c.FormFile("file")
		if err != nil {
			c.String(http.StatusBadRequest, "no .CTP file uploaded")
			return
		}
		src, err := fh.Open()
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer src.Close()

		manifest, mediaFiles, err := parseShowZip(src)
		if err != nil {
			c.String(http.StatusUnprocessableEntity, "invalid .CTP file: "+err.Error())
			return
		}
		if manifest.Version > manifestVersion {
			c.String(http.StatusUnprocessableEntity, fmt.Sprintf("unsupported .CTP version %d (this build reads up to %d)", manifest.Version, manifestVersion))
			return
		}

		// Validate every cue's media is available before mutating anything:
		// either already in the local pool or carried in the zip. A missing
		// source must not leave a half-imported sheet.
		registered, err := registeredFilenames(manifest.Cues)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		for _, cue := range manifest.Cues {
			if !registered[cue.Filename] {
				if _, inZip := mediaFiles[cue.Filename]; !inZip {
					c.String(http.StatusUnprocessableEntity, fmt.Sprintf("cue %q references %q, which is neither in the .CTP nor the local media pool", cue.Title, cue.Filename))
					return
				}
			}
		}

		if mode == "overwrite" {
			if err := ctp.ClearCueSheet(); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
		}

		// Import media: register anything new, in export order so background
		// thumbnail generation picks them up like uploads do.
		mediaDir := config.MediaLocation()
		for _, cue := range manifest.Cues {
			if registered[cue.Filename] {
				continue
			}
			data, ok := mediaFiles[cue.Filename]
			if !ok {
				continue
			}
			dest := filepath.Join(mediaDir, cue.Filename)
			if err := os.WriteFile(dest, data, 0o644); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			meta, err := media.Probe(dest)
			if err != nil {
				os.Remove(dest)
				c.String(http.StatusUnprocessableEntity, fmt.Sprintf("imported media %q failed probing: %v", cue.Filename, err))
				return
			}
			if err := ctp.RegisterMedia(cue.Filename, int64(len(data)), meta, strings.TrimSuffix(cue.Filename, filepath.Ext(cue.Filename))); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
		}

		appendedOffset := 0
		if mode == "append" {
			if count, err := ctp.CueCount(); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			} else {
				appendedOffset = count
			}
		}
		for _, cue := range manifest.Cues {
			if _, err := ctp.AddCueFull(cue); err != nil {
				c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
				return
			}
		}
		ctp.SelectedCuePosFor(manifest.SelectedCuePos, len(manifest.Cues), appendedOffset)

		if c.GetHeader("HX-Request") != "" {
			renderCuesheet(c)
			return
		}
		c.Redirect(http.StatusSeeOther, "/")
	})
}

func registeredFilenames(cues []ctp.ExportCue) (map[string]bool, error) {
	// Group works off the same pool the cues reference; if a .CTP references
	// duplicated filenames, the first registration decides.
	reg := map[string]bool{}
	for _, cue := range cues {
		ok, err := ctp.MediaRegistered(cue.Filename)
		if err != nil {
			return nil, err
		}
		reg[cue.Filename] = ok
	}
	return reg, nil
}

// writeShowZip builds the .CTP archive in-memory: cutepi.json plus one entry
// per referenced media file that exists on disk (missing sources are skipped;
// import rejects cues whose source is absent).
func writeShowZip(dst io.Writer, m showManifest) error {
	zw := zip.NewWriter(dst)

	w, err := zw.Create("cutepi.json")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(m); err != nil {
		return err
	}

	for _, cue := range m.Cues {
		path := filepath.Join(config.MediaLocation(), cue.Filename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		w, err := zw.Create("media/" + cue.Filename)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return zw.Close()
}

// parseShowZip reads cutepi.json and the media/ entries from a .CTP archive.
func parseShowZip(src io.Reader) (showManifest, map[string][]byte, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return showManifest{}, nil, err
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return showManifest{}, nil, err
	}

	var m showManifest
	mediaFiles := map[string][]byte{}
	for _, f := range reader.File {
		switch {
		case f.Name == "cutepi.json":
			rc, err := f.Open()
			if err != nil {
				return showManifest{}, nil, err
			}
			err = json.NewDecoder(rc).Decode(&m)
			rc.Close()
			if err != nil {
				return showManifest{}, nil, err
			}
		case strings.HasPrefix(f.Name, "media/"):
			rc, err := f.Open()
			if err != nil {
				return showManifest{}, nil, err
			}
			content, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return showManifest{}, nil, err
			}
			mediaFiles[strings.TrimPrefix(f.Name, "media/")] = content
		}
	}
	if m.Version == 0 {
		return showManifest{}, nil, fmt.Errorf("missing cutepi.json manifest")
	}
	return m, mediaFiles, nil
}