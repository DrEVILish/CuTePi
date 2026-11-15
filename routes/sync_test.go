package routes

import (
	"fmt"
	"strings"
	"testing"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/media"

	"github.com/gin-gonic/gin"
)

// TestSyncVersionsMatchStatusEndpoints: the versions every WebSocket sync
// carries (syncVersions, ws.OnSync) are exactly what the panes' status
// endpoints report, so a window that compares them with the sheet's
// data-version and the now-playing version fetches the same panes it would
// after asking. A sheet change moves them.
func TestSyncVersionsMatchStatusEndpoints(t *testing.T) {
	r := setupTestServer(t)
	v := syncVersions().(gin.H)
	sheet := ctp.CuesheetVersion()
	if v["sheet"] != sheet || v["state"] != fmt.Sprintf("%d.%d", gsp.StateVersion(), sheet) || v["boot"] != bootID || bootID == "" {
		t.Fatalf("syncVersions = %v, want sheet %d, state %d.%d, boot %q", v, sheet, gsp.StateVersion(), sheet, bootID)
	}
	if body := get(t, r, "/api/cuesheet").Body.String(); !strings.Contains(body, fmt.Sprintf(`data-version="%d"`, sheet)) {
		t.Fatalf("sheet render does not carry version %d", sheet)
	}

	if w := postForm(t, r, "/api/setting/showmode", "showmode", "1"); w.Code >= 300 {
		t.Fatalf("showmode: %d", w.Code)
	}
	after := syncVersions().(gin.H)
	if after["sheet"].(uint64) <= sheet {
		t.Fatalf("show mode switch left the sheet version at %v (was %d): other windows would not follow", after["sheet"], sheet)
	}
	// Other windows learn the mode from the sheet render the sync makes them fetch.
	if body := get(t, r, "/api/cuesheet").Body.String(); !strings.Contains(body, `data-show-mode="true"`) {
		t.Fatal(`sheet render lacks data-show-mode="true" after the switch`)
	}
	postForm(t, r, "/api/setting/showmode", "showmode", "")
}

// TestIndexRendersNowPlayingWithData: the top bar's GO cluster is rendered
// with the page's data on first load (it was rendered with none, so every
// fresh window showed "nothing selected" until its first sync).
func TestIndexRendersNowPlayingWithData(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("sync-test.mp4", 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, "sync-test.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("sync-test.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if w := post(t, r, "/api/cue/1"); w.Code != 200 {
		t.Fatalf("select: %d", w.Code)
	}
	body := get(t, r, "/").Body.String()
	i := strings.Index(body, `class="go-now"`)
	if i < 0 {
		t.Fatal("no GO cluster on the page")
	}
	if seg := body[i : i+40]; strings.Contains(seg, "hidden") {
		t.Fatalf("GO cluster hidden on load with a cue selected: %q", seg)
	}
}
