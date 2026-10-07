package gsp

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/logs"
)

// Pool names may hold characters a URI path cannot: the URI must escape
// them and decode back to the exact absolute path.
func TestFileURIEscapesAndRoundTrips(t *testing.T) {
	for _, name := range []string{"walk in.mp3", "act #2.wav", "what?.flac", "100% hits.mp3", "naïve—song.ogg"} {
		path := filepath.Join("/srv/media", name)
		uri, err := fileURI(path)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(uri)
		if err != nil || u.Scheme != "file" || u.Path != path || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("%q -> %q: parsed scheme %q path %q query %q fragment %q (%v)", name, uri, u.Scheme, u.Path, u.RawQuery, u.Fragment, err)
		}
		if strings.ContainsAny(strings.TrimPrefix(uri, "file://"), " #?") {
			t.Fatalf("%q -> %q: unescaped characters", name, uri)
		}
	}
}

// A playlist none of whose tracks can play stops (logged) after one pass
// instead of retrying in a tight loop.
func TestBackgroundPlaylistDoesNotSpin(t *testing.T) {
	gstInit()
	config.SetDirsForTesting(t.TempDir())
	defer func(d time.Duration) { bgRetryPause = d }(bgRetryPause)
	bgRetryPause = 50 * time.Millisecond
	logs.Clear()
	stop := BackgroundPlaylist([]string{"missing-1.mp3", "missing-2.mp3"}, 0)
	defer stop()
	playlistEnded := func() bool {
		for _, e := range logs.Recorded() {
			if e.Code == logs.GSPBackground && strings.Contains(e.Message, "playlist stopped") {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(5 * time.Second); !playlistEnded() && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // a spinning playlist would log more meanwhile
	failed, ended := 0, 0
	for _, e := range logs.Recorded() {
		if e.Code != logs.GSPBackground {
			continue
		}
		if strings.Contains(e.Message, "playlist stopped") {
			ended++
		} else {
			failed++
		}
	}
	if failed != 2 || ended != 1 {
		t.Fatalf("track failures logged %d, playlist ends %d; want 2 and 1 (one pass, then stop)", failed, ended)
	}
}
