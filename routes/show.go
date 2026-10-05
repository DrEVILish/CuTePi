package routes

import (
	"archive/zip"
	"encoding/json"
	"errors"
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
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
		}
		if len(cues) == 0 {
			c.String(http.StatusBadRequest, "the cuesheet is empty - nothing to export")
			return
		}
		groups, err := ctp.ExportGroups()
		if err != nil {
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
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
			logs.PrintfWarn("EXPORT", "show export failed: %v", err)
			if !c.Writer.Written() {
				c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
				return
			}
			// Mid-stream: the 200 and part of the zip are already out, so an
			// error body would just be appended to the archive. Kill the
			// connection instead — the browser reports a failed download
			// rather than saving a truncated .CTP that looks complete.
			abortConnection(c)
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
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
		}
		defer src.Close()

		// Spool the uploaded .CTP to a temp file rather than io.ReadAll: the
		// archive is streamed out of it entry-at-a-time below, so a big show
		// never occupies RAM on the Pi. Spooled under the data dir's tmp/,
		// not /tmp, which is a RAM-backed tmpfs on current Raspberry Pi OS.
		if err := os.MkdirAll(config.TmpDir(), 0o755); err != nil {
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
		}
		tmpZip, err := os.CreateTemp(config.TmpDir(), "show-*.ctp")
		if err != nil {
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
		}
		zipPath := tmpZip.Name()
		if _, err := io.Copy(tmpZip, src); err != nil {
			tmpZip.Close()
			os.Remove(zipPath)
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
		}
		if err := tmpZip.Close(); err != nil {
			os.Remove(zipPath)
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
		}
		defer os.Remove(zipPath)

		manifest, mediaFiles, err := parseShowZip(zipPath)
		if err != nil {
			status := http.StatusUnprocessableEntity
			if errors.Is(err, errShowTooLarge) {
				status = http.StatusInsufficientStorage
			}
			c.String(status, "%s", redactPaths("invalid .CTP file: "+err.Error()))
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
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
		}
		for _, cue := range manifest.Cues {
			if cue.SourceKind == "endpoint" {
				continue
			}
			if !registered[cue.Filename] {
				if _, inZip := mediaFiles[cue.Filename]; !inZip {
					c.String(http.StatusUnprocessableEntity, fmt.Sprintf("cue %q references %q, which is neither in the .CTP nor the local media pool", cue.Title, cue.Filename))
					return
				}
			}
		}

		// Import media BEFORE touching the cuesheet: a media write/probe
		// failure must not cost the operator their current show. The .CTP's
		// media entries were streamed to temp files during parsing;
		// importMedia moves each into place (rename, or copy across mounts).
		for _, cue := range manifest.Cues {
			if cue.SourceKind == "endpoint" {
				continue
			}
			if registered[cue.Filename] {
				continue
			}
			tmp, ok := mediaFiles[cue.Filename]
			if !ok {
				continue
			}
			delete(mediaFiles, cue.Filename) // importMedia owns (and cleans up) tmp now
			if err := importMedia(cue.Filename, tmp); err != nil {
				c.String(http.StatusUnprocessableEntity, "imported media %q failed: %v", cue.Filename, err)
				return
			}
		}

		// Only after the media is in place: overwrite clears the sheet (and
		// any local groups — stale folders would otherwise survive the
		// import). This must run BEFORE ImportGroups: ClearCueSheet deletes
		// every group, so clearing afterwards dropped the show's own groups
		// and left its cues pointing at deleted parents. If an insert below
		// fails mid-way, the cues and groups inserted so far are rolled back
		// so the sheet is left empty-but-consistent (with an audit note)
		// instead of a random half-show.
		appendedOffset := 0
		switch mode {
		case "overwrite":
			if err := ctp.ClearCueSheet(); err != nil {
				c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
				return
			}
		case "append":
			count, err := ctp.CueCount()
			if err != nil {
				c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
				return
			}
			appendedOffset = count
		}

		// Groups are created before cues: cue Parent values reference group
		// ids, and the manifest's ids are remapped through the returned map
		// (imported groups get fresh local ids). v1 manifests have no groups,
		// leaving the map empty and every cue top-level — matching pre-group
		// behaviour.
		idMap, err := ctp.ImportGroups(manifest.Groups)
		if err != nil {
			c.String(http.StatusInternalServerError, "%s", redactPaths(err.Error()))
			return
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
				respondError(c, http.StatusInternalServerError, err.Error())
				return
			}
			inserted++
		}
		ctp.SelectedCuePosFor(manifest.SelectedCuePos, len(manifest.Cues), appendedOffset)
		logs.Emit(logs.AuditEvent{Event: "show_imported", Title: fmt.Sprintf("%s: %d cues", mode, inserted)})

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

	written := map[string]bool{}
	for _, cue := range m.Cues {
		// Live pages carry their URL in the manifest and have no file.
		if cue.SourceKind == "endpoint" || cue.Filename == "" {
			continue
		}
		// One entry per file: cues sharing media must not duplicate it.
		if written[cue.Filename] {
			continue
		}
		written[cue.Filename] = true
		path := filepath.Join(config.MediaLocation(), cue.Filename)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		// Stored, not deflated: media is already compressed, and deflating
		// a multi-GB show burns the Pi's CPU (possibly mid-show) for ~0 gain.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "media/" + cue.Filename, Method: zip.Store, Modified: time.Now()})
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

// errShowTooLarge: the archive's media would not fit on the media volume.
var errShowTooLarge = errors.New("not enough free space for the show's media")

// parseShowZip reads cutepi.json and the media/ entries from a .CTP archive
// on disk. Entry names are validated with safeMediaName before they ever
// reach filepath.Join on import. Media content is streamed out to temp files
// (returned as filename -> temp path), so a multi-GB show never sits in RAM.
// On error every temp file it created is removed and the map is nil.
//
// Size guard: the sum of the entries' declared uncompressed sizes must fit
// in the media volume's free space (with headroom). archive/zip refuses to
// inflate an entry past its declared size, so a zip bomb cannot exceed the
// total checked here.
func parseShowZip(path string) (_ showManifest, _ map[string]string, err error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return showManifest{}, nil, err
	}
	defer zr.Close()

	// Duplicate media/ entries are legitimate (older exports wrote one
	// entry per cue, so cues sharing a file repeat it); only the first of
	// each name is extracted or counted.
	var total uint64
	counted := map[string]bool{}
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "media/") && !counted[f.Name] {
			counted[f.Name] = true
			total += f.UncompressedSize64
		}
	}
	if free, ok := mediaFreeBytes(); ok {
		const headroom = 256 << 20 // keep the DB and logs writable
		if total+headroom > free {
			return showManifest{}, nil, fmt.Errorf("%w: needs %d MiB, %d MiB free", errShowTooLarge, total>>20, free>>20)
		}
	}

	var m showManifest
	mediaFiles := map[string]string{}
	defer func() {
		if err != nil {
			removeMediaTemps(mediaFiles)
		}
	}()
	for _, f := range zr.File {
		switch {
		case f.Name == "cutepi.json":
			rc, err := f.Open()
			if err != nil {
				return showManifest{}, nil, err
			}
			err = json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&m)
			rc.Close()
			if err != nil {
				return showManifest{}, nil, err
			}
		case strings.HasPrefix(f.Name, "media/"):
			name := strings.TrimPrefix(f.Name, "media/")
			if !safeMediaName(name) {
				return showManifest{}, nil, fmt.Errorf("unsafe media entry name %q", f.Name)
			}
			if _, dup := mediaFiles[name]; dup {
				continue // same file, already extracted
			}
			tmp, err := extractToTemp(f)
			if err != nil {
				return showManifest{}, nil, err
			}
			mediaFiles[name] = tmp
		}
	}
	if m.Version == 0 {
		return showManifest{}, nil, fmt.Errorf("missing cutepi.json manifest")
	}
	for _, cue := range m.Cues {
		// Live pages have no file: check the URL now, before an overwrite
		// import clears the sheet.
		if cue.SourceKind == "endpoint" {
			if err := ctp.ValidateEndpointURL(cue.EndpointURL); err != nil {
				return showManifest{}, nil, fmt.Errorf("manifest cue %q: %w", cue.Title, err)
			}
			continue
		}
		if !safeMediaName(cue.Filename) {
			return showManifest{}, nil, fmt.Errorf("manifest cue %q has an unsafe media filename %q", cue.Title, cue.Filename)
		}
	}
	return m, mediaFiles, nil
}

// extractToTemp streams one archive entry to a new temp file under the data
// dir's tmp/ and returns its path (removed again on any failure).
func extractToTemp(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	if err := os.MkdirAll(config.TmpDir(), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(config.TmpDir(), "import-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// abortConnection hard-closes the client connection. Used when a streamed
// response fails after its status line went out, so the client sees a
// broken transfer instead of a truncated body that looks complete.
func abortConnection(c *gin.Context) {
	c.Abort()
	if conn, _, err := c.Writer.Hijack(); err == nil {
		_ = conn.Close()
	}
}
