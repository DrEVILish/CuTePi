package routes

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"CuTePi/config"
)

// Concurrent renames of one taken name each get their own free name.
func TestReserveMediaNameConcurrentRename(t *testing.T) {
	setupTestServer(t)
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "clip.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	const n = 16
	names := make([]string, n)
	releases := make([]func(), n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name, release, err := reserveMediaName("clip.mp4", reserveRename)
			if err != nil {
				t.Error(err)
				return
			}
			names[i], releases[i] = name, release
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, nm := range names {
		if nm == "clip.mp4" || seen[nm] {
			t.Fatalf("reserved names %v: %q given twice or the taken name", names, nm)
		}
		seen[nm] = true
	}
	for _, r := range releases {
		r()
	}
	// Exactly one of several concurrent claims on a free name wins.
	wins := 0
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, release, err := reserveMediaName("new.mp4", reserveNew); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
				t.Cleanup(release) // held until the end: the others must keep losing
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d concurrent reserveNew claims won, want 1", wins)
	}
}

// Two simultaneous rename-uploads of the same taken name keep both files.
func TestConcurrentRenameUploadsKeepBoth(t *testing.T) {
	r := setupTestServer(t)
	if w := uploadWav(t, r, "dup.wav", "", 1); w.Code >= 400 {
		t.Fatalf("first upload = %d: %s", w.Code, w.Body.String())
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = uploadWav(t, r, "dup.wav", "rename", 1+i).Code
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c >= 400 && c != http.StatusSeeOther {
			t.Fatalf("rename uploads = %v", codes)
		}
	}
	entries, _ := os.ReadDir(config.MediaLocation())
	var got []string
	for _, e := range entries {
		if !e.IsDir() { // the staging tmp/ folder
			got = append(got, e.Name())
		}
	}
	sort.Strings(got)
	want := []string{"dup (2).wav", "dup (3).wav", "dup.wav"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("media folder = %v, want %v", got, want)
	}
}
