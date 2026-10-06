package routes

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/ctp"
)

// addLive adds a live-page cue through the clock button's route and returns
// the new (selected) cue.
func addLive(t *testing.T, r *gin.Engine, title, url string) ctp.Cue {
	t.Helper()
	w := postForm(t, r, "/api/cue/live", "title", title, "url", url)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/live = %d: %s", w.Code, w.Body.String())
	}
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		t.Fatalf("new live cue not selected (pos %d, %v)", pos, err)
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil {
		t.Fatal(err)
	}
	return cue
}

// The clock button adds a cue straight to the sheet; live pages never show
// up as pool items (pool view, HyperDeck clip list, media worker).
func TestLiveCueAddsToSheetNotPool(t *testing.T) {
	r := setupTestServer(t)
	cue := addLive(t, r, "Room A", "https://timer.example/d/")
	if cue.SourceKind != "endpoint" || cue.EndpointURL != "https://timer.example/d/" || cue.Title != "Room A" {
		t.Fatalf("live cue = kind %q url %q title %q", cue.SourceKind, cue.EndpointURL, cue.Title)
	}
	if cue.FadeIn != ctp.LiveCueFadeInMs {
		t.Fatalf("live cue fade-in = %v, want %d ms", cue.FadeIn, ctp.LiveCueFadeInMs)
	}
	if !strings.Contains(get(t, r, "/api/cuesheet").Body.String(), "Room A") {
		t.Fatal("cuesheet does not show the live cue")
	}
	pool, err := ctp.GetMediapool()
	if err != nil || len(pool.Medias) != 0 {
		t.Fatalf("pool = %d items (%v), want none", len(pool.Medias), err)
	}
	if body := get(t, r, "/mediapool").Body.String(); strings.Contains(body, "timer.example") {
		t.Fatal("live page rendered in the media pool")
	}

	// Blank name: the page's host.
	if c := addLive(t, r, "", "http://timerpi.local:8080/d/room"); c.Title != "timerpi.local:8080" {
		t.Fatalf("blank-name title = %q, want the host", c.Title)
	}
}

func TestLiveCueRejectsBadURL(t *testing.T) {
	r := setupTestServer(t)
	for _, u := range []string{"", "file:///etc/passwd", "javascript:alert(1)", "timer.local/d", "http://user:pw@timer.local/"} {
		if w := postForm(t, r, "/api/cue/live", "url", u); w.Code != http.StatusBadRequest {
			t.Errorf("url %q = %d, want 400", u, w.Code)
		}
	}
	if n, _ := ctp.CueCount(); n != 0 {
		t.Fatalf("%d cues added by rejected URLs", n)
	}
	if err := ctp.PruneEndpoints(); err != nil {
		t.Fatal(err)
	}
}

// The inspector shows the live-page controls (URL, fades, waits, schedule)
// and none of the file-only ones (trim, loop, rate, waveform, audio).
func TestLiveCueInspector(t *testing.T) {
	r := setupTestServer(t)
	addLive(t, r, "Room A", "https://timer.example/d/")
	body := get(t, r, "/api/cue/inspector").Body.String()
	for _, want := range []string{`name="endpointUrl"`, "https://timer.example/d/", `name="preWait"`, `name="schedule_enabled"`} {
		if !strings.Contains(body, want) {
			t.Errorf("inspector lacks %s", want)
		}
	}
	for _, unwanted := range []string{`name="posStart"`, `name="loop"`, `name="rate"`, `id="trim-timeline"`, `name="volume"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("inspector shows file-only control %s", unwanted)
		}
	}
}

// Editing one live cue's URL leaves another cue on the same page alone.
func TestLiveCueURLEditIsPerCue(t *testing.T) {
	r := setupTestServer(t)
	a := addLive(t, r, "A", "https://timer.example/d/")
	b := addLive(t, r, "B", "https://timer.example/d/")
	path := "/api/cue/inspector/" + strconv.Itoa(a.CuePos)
	if w := putForm(t, r, path, "endpointUrl", "https://timer.example/d/room-b"); w.Code != http.StatusOK {
		t.Fatalf("inspector save = %d: %s", w.Code, w.Body.String())
	}
	if got, _ := ctp.GetCue(strconv.Itoa(a.CuePos)); got.EndpointURL != "https://timer.example/d/room-b" {
		t.Fatalf("edited cue URL = %q", got.EndpointURL)
	}
	if got, _ := ctp.GetCue(strconv.Itoa(b.CuePos)); got.EndpointURL != "https://timer.example/d/" {
		t.Fatalf("other cue's URL changed to %q", got.EndpointURL)
	}
	if w := putForm(t, r, path, "endpointUrl", "ftp://nope"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad URL save = %d, want 400", w.Code)
	}
}

// Live cues survive a .CTP round trip with their URL.
func TestLiveCueExportImport(t *testing.T) {
	r := setupTestServer(t)
	addLive(t, r, "Room A", "https://timer.example/d/")
	exp := get(t, r, "/api/show/export")
	if exp.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", exp.Code, exp.Body.String())
	}
	if w := postCTP(t, r, exp.Body.Bytes(), "overwrite"); w.Code != http.StatusSeeOther {
		t.Fatalf("import = %d: %s", w.Code, w.Body.String())
	}
	cs, err := ctp.GetCuesheet()
	if err != nil || len(cs.Cues) != 1 {
		t.Fatalf("cues after import = %d (%v)", len(cs.Cues), err)
	}
	if c := cs.Cues[0]; c.SourceKind != "endpoint" || c.EndpointURL != "https://timer.example/d/" {
		t.Fatalf("imported live cue = kind %q url %q", c.SourceKind, c.EndpointURL)
	}
	// The overwritten show's source row is now orphaned; pruning drops it
	// and keeps the imported one.
	if err := ctp.PruneEndpoints(); err != nil {
		t.Fatal(err)
	}
	if c, _ := ctp.GetCue(strconv.Itoa(cs.Cues[0].CuePos)); c.EndpointURL == "" {
		t.Fatal("pruning removed the live cue's own source")
	}
}

// A scheduled live cue must not break the scheduler's due-cue query (its
// source has no filename).
func TestScheduledLiveCueIsQueryable(t *testing.T) {
	r := setupTestServer(t)
	cue := addLive(t, r, "Room A", "https://timer.example/d/")
	now := time.Now()
	day := int(now.Weekday())
	if day == 0 {
		day = 7
	}
	if err := ctp.SetCueSchedule(cue.CuePos, true, day, now.Hour()*3600+now.Minute()*60+now.Second()); err != nil {
		t.Fatal(err)
	}
	rows, err := ctp.GetScheduledCues(now)
	if err != nil {
		t.Fatalf("GetScheduledCues: %v", err)
	}
	if len(rows) != 1 || rows[0].SourceKind != "endpoint" || rows[0].EndpointURL != "https://timer.example/d/" {
		t.Fatalf("scheduled rows = %+v", rows)
	}
}

// The cache keeper (run here without WebKit) deletes the sources no cue uses
// (deleted cue, changed URL) and keeps the ones cues still use.
func TestLiveCacheKeeperPrunesOrphans(t *testing.T) {
	r := setupTestServer(t)
	a := addLive(t, r, "A", "https://timer.example/d/a")
	b := addLive(t, r, "B", "https://other.example/d/b")
	if err := ctp.SetCueEndpointURL(a.CuePos, "https://timer.example/d/a2"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.SetCueEndpointURL(a.CuePos, "https://timer.example/d/a2"); err != nil { // unchanged: no new row
		t.Fatal(err)
	}
	if w := del(t, r, "/api/cue/"+strconv.Itoa(b.CuePos)); w.Code != http.StatusOK {
		t.Fatalf("delete cue = %d", w.Code)
	}
	orphans, err := ctp.OrphanEndpoints()
	if err != nil || len(orphans) != 2 {
		t.Fatalf("orphans before keeper = %+v (%v), want the old A source and B's", orphans, err)
	}
	if got := sourceHosts(orphans); strings.Join(got, ",") != "other.example,timer.example" {
		t.Fatalf("orphan hosts = %v", got)
	}
	keepLiveCache(false)
	if orphans, _ := ctp.OrphanEndpoints(); len(orphans) != 0 {
		t.Fatalf("orphans after keeper = %+v", orphans)
	}
	live, _ := ctp.LiveSources()
	if len(live) != 1 || live[0].URL != "https://timer.example/d/a2" {
		t.Fatalf("live sources = %+v", live)
	}
	if got, _ := ctp.GetCue(strconv.Itoa(a.CuePos)); got.EndpointURL != "https://timer.example/d/a2" || got.Title != "A" {
		t.Fatalf("cue A after keeper = url %q title %q", got.EndpointURL, got.Title)
	}
}
