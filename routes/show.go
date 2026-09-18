package routes

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/logs"
)

// manifestVersion is the format version of cutepi.json inside a .CTP file.
// v2 adds the groups table (membership, nesting, slideshow settings); v1
// manifests import fine — their cue Parent values resolve to nothing and
// every cue lands top-level, matching pre-group behaviour.
const manifestVersion = 2

// showManifest is cutepi.json: every cue and group plus the audit trail and
// the exported selection, so import can rebuild order, structure, selection,
// and history.
type showManifest struct {
	App            string            `json:"app"`
	Version        int               `json:"version"`
	ExportedAt     string            `json:"exportedAt"`
	SelectedCuePos int               `json:"selectedCuePos"`
	Cues           []ctp.ExportCue   `json:"cues"`
	Groups         []ctp.ExportGroup `json:"groups,omitempty"`
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
		groups, err := ctp.ExportGroups()
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}

		filename := fmt.Sprintf("show-%s.ctp", time.Now().Format("20060102-150405"))
		c.Header("Content-Type", "application/zip")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		// Stream the zip to the response instead of building it in a buffer:
		// media is copied entry-at-a-time, so a multi-GB show never sits in
		// RAM on the Pi.
		if err := writeShowZip(c.Writer, showManifest{
			App:            "CuTePi",
			Version:        manifestVersion,
			ExportedAt:     time.Now().Format(time.RFC3339),
			SelectedCuePos: selected,
			Cues:           cues,
			Groups:         groups,
			Audit:          logs.AuditTrail(),
		}); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
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

		// Spool the uploaded .CTP to a temp file rather than io.ReadAll: the
		// archive is streamed out of it entry-at-a-time below, so a big show
		// never occupies RAM on the Pi.
		tmpZip, err := os.CreateTemp("", "cutepi-show-*.ctp")
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		zipPath := tmpZip.Name()
		if _, err := io.Copy(tmpZip, src); err != nil {
			tmpZip.Close()
			os.Remove(zipPath)
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		if err := tmpZip.Close(); err != nil {
			os.Remove(zipPath)
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		defer os.Remove(zipPath)

		manifest, mediaFiles, err := parseShowZip(zipPath)
		if err != nil {
			removeMediaTemps(mediaFiles)
			c.String(http.StatusUnprocessableEntity, "invalid .CTP file: "+err.Error())
			return
		}
		defer removeMediaTemps(mediaFiles)
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

		// Import media BEFORE touching the cuesheet: a media write/probe
		// failure must not cost the operator their current show. The .CTP's
		// media entries were streamed to temp files during parsing; commit
		// here moves each into place (copy+rename so it works across
		// filesystems; /tmp and the media dir may be different mounts).
		mediaDir := config.MediaLocation()
		for _, cue := range manifest.Cues {
			if registered[cue.Filename] {
				continue
			}
			tmp, ok := mediaFiles[cue.Filename]
			if !ok {
				continue
			}
			dest := filepath.Join(mediaDir, cue.Filename)
			if err := moveIntoPlace(tmp, dest); err != nil {
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			delete(mediaFiles, cue.Filename) // committed; skip deferred cleanup
			if err := importMedia(cue.Filename, dest); err != nil {
				c.String(http.StatusUnprocessableEntity, fmt.Sprintf("imported media %q failed: %v", cue.Filename, err))
				return
			}
		}

		// Groups are created before cues: cue Parent values reference group
		// ids, and the manifest's ids are remapped through the returned map
		// (imported groups get fresh local ids). v1 manifests have no groups,
		// leaving the map empty and every cue top-level — matching pre-group
		// behaviour.
		idMap, err := ctp.ImportGroups(manifest.Groups)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}

		appendedOffset := 0
		if mode == "append" {
			if count, err := ctp.CueCount(); err != nil {
				ctp.ImportGroupsRollback(idMap)
				c.String(http.StatusInternalServerError, err.Error())
				return
			} else {
				appendedOffset = count
			}
		}

		// Only after the media is in place: overwrite clears the sheet (and
		// any local groups — stale folders would otherwise survive the
		// import), then cues are inserted. If an insert fails mid-way, the
		// cues and groups inserted so far are rolled back so the sheet is
		// left empty-but-consistent (with an audit note) instead of a random
		// half-show.
		if mode == "overwrite" {
			if err := ctp.ClearCueSheet(); err != nil {
				ctp.ImportGroupsRollback(idMap)
				c.String(http.StatusInternalServerError, err.Error())
				return
			}
			appendedOffset = 0
		}
		inserted := 0
		for _, cue := range manifest.Cues {
			cue.Parent = idMap[cue.Parent]
			if _, err := ctp.AddCueFull(cue); err != nil {
				// Roll back only this import's inserts. AddCueFull always
				// appends (it ignores the exported cuePos), so in append mode
				// the inserts sit at appendedOffset+1.., never at the top of
				// the operator's existing sheet.
				for pos := appendedOffset + inserted; pos > appendedOffset; pos-- {
					_ = ctp.RemoveCue(strconv.Itoa(pos)) // best-effort rollback of this import's inserts
				}
				ctp.ImportGroupsRollback(idMap)
				logs.Emit(logs.AuditEvent{Event: "import_failed", Pos: 0, Title: fmt.Sprintf("after %d cues: %v", inserted, err)})
				c.HTML(http.StatusInternalServerError, "error.html", gin.H{"error": err.Error()})
				return
			}
			inserted++
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

// writeShowZip writes the .CTP archive to dst, streaming: cutepi.json plus
// one entry per referenced media file that exists on disk (missing sources
// are skipped; import rejects cues whose source is absent). dst is usually
// the HTTP response writer, so peak memory is one archive entry's buffer
// rather than the whole show.
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
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		w, err := zw.Create("media/" + cue.Filename)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(w, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return zw.Close()
}

// safeMediaName rejects anything that is not a plain relative filename:
// empty, path separators, or traversal components. Export always writes
// bare filenames ("media/clip.mp4"), so anything else in a zip entry name or
// manifest cue is either foreign or malicious (zip-slip would otherwise let
// a crafted .CTP write outside the media dir via filepath.Join).
func safeMediaName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	return filepath.Base(name) == name && name != "." && name != ".."
}

// removeMediaTemps best-effort removes the temp files parseShowZip created,
// ignoring ones already committed (renamed into place or removed).
func removeMediaTemps(mediaFiles map[string]string) {
	for _, tmp := range mediaFiles {
		if tmp != "" {
			os.Remove(tmp)
		}
	}
}

// moveIntoPlace moves tmp to dest, copying across filesystems when rename
// can't (os.CreateTemp spools to /tmp, which may be a different mount than
// the media dir). Always streams, never buffers the file in RAM.
func moveIntoPlace(tmp, dest string) error {
	if err := os.Rename(tmp, dest); err == nil {
		return nil
	}
	in, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(tmp)
}

// parseShowZip reads cutepi.json and the media/ entries from a .CTP archive
// on disk. Entry names are validated with safeMediaName before they ever
// reach filepath.Join on import. Media content is streamed out to temp files
// (returned as filename -> temp path), so a multi-GB show never sits in RAM.
func parseShowZip(path string) (showManifest, map[string]string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return showManifest{}, nil, err
	}
	defer zr.Close()

	var m showManifest
	mediaFiles := map[string]string{}
	for _, f := range zr.File {
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
			name := strings.TrimPrefix(f.Name, "media/")
			if !safeMediaName(name) {
				return showManifest{}, nil, fmt.Errorf("unsafe media entry name %q", f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return showManifest{}, nil, err
			}
			tmp, err := os.CreateTemp("", "cutepi-media-*")
			if err != nil {
				rc.Close()
				return showManifest{}, nil, err
			}
			if _, err := io.Copy(tmp, rc); err != nil {
				rc.Close()
				tmp.Close()
				os.Remove(tmp.Name())
				return showManifest{}, nil, err
			}
			rc.Close()
			if err := tmp.Close(); err != nil {
				os.Remove(tmp.Name())
				return showManifest{}, nil, err
			}
			mediaFiles[name] = tmp.Name()
		}
	}
	if m.Version == 0 {
		removeMediaTemps(mediaFiles)
		return showManifest{}, nil, fmt.Errorf("missing cutepi.json manifest")
	}
	for _, cue := range m.Cues {
		if !safeMediaName(cue.Filename) {
			removeMediaTemps(mediaFiles)
			return showManifest{}, nil, fmt.Errorf("manifest cue %q has an unsafe media filename %q", cue.Title, cue.Filename)
		}
	}
	return m, mediaFiles, nil
}