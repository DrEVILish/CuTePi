package routes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/logs"
)

// Media of an Active/active backup pair (DESIGN §6.8c). Each unit lists its
// pool by file name, size and content hash; units whose pools differ show
// which files are missing where. Files are copied unit to unit through the
// receiving unit's own /upload (probed and registered like any upload):
// on the operator's "Sync media", and for every upload or download made on
// one unit once it has finished.

// mediaFile is one pool file as a peer sees it.
type mediaFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}

// mediaDiff is how this unit's pool differs from a peer's.
type mediaDiff struct {
	Missing []string // here, not on the peer
	Lacking []string // on the peer, not here
	Differs []string // on both, other content
}

// Bad reports whether the pools differ.
func (d mediaDiff) Bad() bool { return len(d.Missing)+len(d.Lacking)+len(d.Differs) > 0 }

// hashSample is how much of each end of a file the hash reads.
const hashSample = 512 << 10

var (
	hashMu    sync.Mutex
	hashCache = map[string]hashEntry{}
)

type hashEntry struct {
	size  int64
	mtime time.Time
	hash  string
}

// fileHash is the SHA-256 of a file's size, first and last 512 KiB, cached
// by size and mtime.
// shortcut: a sampled hash (a whole-file hash of a show's media reads GBs off
// the SD card); an edit in the middle of a large file is not seen. Hash the
// whole file if that ever matters.
func fileHash(path string, info os.FileInfo) (string, error) {
	hashMu.Lock()
	e, ok := hashCache[path]
	hashMu.Unlock()
	if ok && e.size == info.Size() && e.mtime.Equal(info.ModTime()) {
		return e.hash, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	fmt.Fprintf(h, "%d\n", info.Size())
	if _, err := io.CopyN(h, f, hashSample); err != nil && err != io.EOF {
		return "", err
	}
	if info.Size() > 2*hashSample {
		if _, err := f.Seek(-hashSample, io.SeekEnd); err != nil {
			return "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	} else if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	hashMu.Lock()
	hashCache[path] = hashEntry{info.Size(), info.ModTime(), sum}
	hashMu.Unlock()
	return sum, nil
}

// mediaList lists this unit's pool files present on disk.
func mediaList() ([]mediaFile, error) {
	pool, err := ctp.GetMediapool()
	if err != nil {
		return nil, err
	}
	out := []mediaFile{}
	for _, m := range pool.Medias {
		if m.Missing || m.Filename == "" {
			continue
		}
		path := filepath.Join(config.MediaLocation(), m.Filename)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		sum, err := fileHash(path, info)
		if err != nil {
			continue
		}
		out = append(out, mediaFile{m.Filename, info.Size(), sum})
	}
	return out, nil
}

// diffMedia compares this unit's files with a peer's.
func diffMedia(mine, theirs []mediaFile) mediaDiff {
	var d mediaDiff
	other := map[string]mediaFile{}
	for _, f := range theirs {
		other[f.Name] = f
	}
	for _, f := range mine {
		o, ok := other[f.Name]
		switch {
		case !ok:
			d.Missing = append(d.Missing, f.Name)
		case o.Hash != f.Hash:
			d.Differs = append(d.Differs, f.Name)
		}
		delete(other, f.Name)
	}
	for n := range other {
		d.Lacking = append(d.Lacking, n)
	}
	sort.Strings(d.Lacking)
	return d
}

// refreshMediaDiff compares the pool with every paired peer's.
func refreshMediaDiff() {
	mine, err := mediaList()
	if err != nil {
		return
	}
	link.mu.Lock()
	var peers []*peerState
	for _, st := range link.peers {
		if st.Secret != "" {
			peers = append(peers, st)
		}
	}
	link.mu.Unlock()
	for _, st := range peers {
		var theirs []mediaFile
		if err := peerJSON(st.LinkPeer, forwardClient, http.MethodGet, "/api/peer/media", nil, &theirs); err != nil {
			continue
		}
		d := diffMedia(mine, theirs)
		link.mu.Lock()
		st.media = d
		link.mu.Unlock()
	}
}

// syncMedia ("Sync media"): copy every file a peer lacks to it and ask it
// for every file this unit lacks. Files whose content differs are left for
// the operator (the warning lists them).
func syncMedia() {
	refreshMediaDiff()
	link.mu.Lock()
	type job struct {
		id      string
		missing []string
		lacking []string
	}
	var jobs []job
	for _, st := range link.peers {
		if st.Secret != "" {
			jobs = append(jobs, job{st.ID, st.media.Missing, st.media.Lacking})
		}
	}
	link.syncNote = "Syncing media…"
	link.mu.Unlock()
	for _, j := range jobs {
		link.mu.Lock()
		st := link.peers[j.id]
		link.mu.Unlock()
		if st == nil {
			continue
		}
		asked := ""
		if len(j.lacking) > 0 {
			if err := peerJSON(st.LinkPeer, peerClient, http.MethodPost, "/api/peer/pushmedia", map[string][]string{"names": j.lacking}, nil); err != nil {
				logs.PrintfWarn(logs.LNKWarn, "asking %s for media: %v", st.Name, err)
			} else {
				asked = fmt.Sprintf(" %s is sending %d file(s) here.", st.Name, len(j.lacking))
			}
		}
		pushFiles(j.id, j.missing)
		link.mu.Lock()
		link.syncNote = "Media sync: copies from here done " + time.Now().Format("15:04:05") + "." + asked
		link.mu.Unlock()
	}
	refreshMediaDiff()
}

// pushFiles copies pool files to the peer id, one at a time.
func pushFiles(id string, names []string) {
	for i, name := range names {
		link.mu.Lock()
		st := link.peers[id]
		if st != nil {
			link.syncNote = fmt.Sprintf("Copying %s to %s (%d of %d)…", name, st.Name, i+1, len(names))
		}
		link.mu.Unlock()
		if st == nil {
			return
		}
		start := time.Now()
		if err := pushFile(st.LinkPeer, name); err != nil {
			logs.PrintfWarn(logs.LNKWarn, "copying %s to %s: %v", name, st.Name, err)
			continue
		}
		logs.Printf(logs.LNKInfo, "copied %s to %s in %.1f s", name, st.Name, time.Since(start).Seconds())
	}
	goSafe(refreshMediaDiff)
}

// pushFile uploads one pool file to p's /upload (a name already there is
// skipped, never replaced).
func pushFile(p ctp.LinkPeer, name string) error {
	if !safeMediaName(name) {
		return fmt.Errorf("invalid file name")
	}
	f, err := os.Open(filepath.Join(config.MediaLocation(), name))
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := mw.WriteField("onConflict", "skip")
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile("media", name); err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	resp, err := peerDo(context.Background(), p, &http.Client{CheckRedirect: noRedirect}, http.MethodPost, "/upload?linked=1", pr, mw.FormDataContentType())
	if err != nil {
		pr.CloseWithError(err)
		return fmt.Errorf("%s", redactURLErr(err, p.URL))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, msg)
	}
	return nil
}

// linkMediaAdded copies files just uploaded or downloaded here to the
// backup pair's other units.
func linkMediaAdded(names []string) {
	if linkMode() != linkModeBackup || len(names) == 0 {
		return
	}
	link.mu.Lock()
	var ids []string
	for id, st := range link.peers {
		if st.Secret != "" {
			ids = append(ids, id)
		}
	}
	link.mu.Unlock()
	for _, id := range ids {
		goSafe(func() { pushFiles(id, names) })
	}
}
