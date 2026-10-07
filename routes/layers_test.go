package routes

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/media"
)

// layerSheet makes n video cues (numbers 1..n) on a fresh server.
func layerSheet(t *testing.T, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		name := "layer" + string(rune('0'+i)) + ".mp4"
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, name); err != nil {
			t.Fatal(err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// New cues stop the others (as before) and go on top; the inspector's Stop
// others switch and Layer control persist, "under" by cue number.
func TestCueLayerSettings(t *testing.T) {
	r := setupTestServer(t)
	layerSheet(t, 3)
	cue, err := ctp.GetCue("2")
	if err != nil {
		t.Fatal(err)
	}
	if !cue.StopOthers || cue.Layer != "top" || cue.LayerUnder != 0 {
		t.Fatalf("defaults: stop_others %v layer %q under %d; want true, top, 0", cue.StopOthers, cue.Layer, cue.LayerUnder)
	}

	// The form carries its marker and an unticked box: Stop others off.
	if w := putForm(t, r, "/api/cue/inspector/2", "stop_others_shown", "1", "layer", "under", "layer_under", "1"); w.Code != 200 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	cue, _ = ctp.GetCue("2")
	first, _ := ctp.GetCue("1")
	if cue.StopOthers || cue.Layer != "under" || cue.LayerUnder != first.Cue_id || cue.LayerUnderNum != first.CueNum {
		t.Fatalf("after save: %+v", cue)
	}
	// A save without the marker (another tab's partial form) leaves it alone.
	if w := putForm(t, r, "/api/cue/inspector/2", "volume", "-3"); w.Code != 200 {
		t.Fatalf("partial PUT = %d: %s", w.Code, w.Body.String())
	}
	if cue, _ = ctp.GetCue("2"); cue.StopOthers {
		t.Fatal("a save without the Stop others marker turned it back on")
	}
	// Ticked: on again.
	putForm(t, r, "/api/cue/inspector/2", "stop_others_shown", "1", "stop_others", "on")
	if cue, _ = ctp.GetCue("2"); !cue.StopOthers {
		t.Fatal("ticking Stop others did not turn it on")
	}

	// The "under" link is to the cue, not its position: it follows a move.
	if _, err := ctp.ReorderCues([]int{2, 3, 1}); err != nil {
		t.Fatal(err)
	}
	var under ctp.Cue
	sheet, _ := ctp.GetCuesheet()
	for _, c := range sheet.Cues {
		if c.Cue_id == cue.Cue_id {
			under = c
		}
	}
	firstPos := ctp.CuePosByID(first.Cue_id)
	if under.LayerUnder != first.Cue_id || cueOpts(under, false).UnderCuePos != firstPos || firstPos == 0 {
		t.Fatalf("under link after a move: %+v (cue %s at %d)", under, first.CueNum, firstPos)
	}

	// Rejections: an unknown layer, an unknown cue, the cue itself.
	for _, kv := range [][]string{{"layer", "middle"}, {"layer_under", "99"}, {"layer_under", under.CueNum}} {
		if w := putForm(t, r, "/api/cue/inspector/"+fmt.Sprint(under.CuePos), kv...); w.Code != http.StatusBadRequest {
			t.Errorf("%s=%s: %d, want 400", kv[0], kv[1], w.Code)
		}
	}

	// The Video tab shows the controls with the stored values.
	post(t, r, "/api/cue/"+fmt.Sprint(under.CuePos))
	body := get(t, r, "/api/cue/inspector").Body.String()
	for _, want := range []string{`name="layer"`, `name="layer_under"`, `value="` + first.CueNum + `"`, `name="stop_others"`, `name="stop_others_shown"`} {
		if !strings.Contains(body, want) {
			t.Errorf("inspector missing %s", want)
		}
	}
}

// Opacity, geometry and crop reach playback for every fire, not only the
// scheduler's (GetCue did not load them until 2026-10-07).
func TestCuePictureSettingsReachPlayback(t *testing.T) {
	r := setupTestServer(t)
	layerSheet(t, 1)
	if w := putForm(t, r, "/api/cue/inspector/1", "opacity", "37", "geom_x", "10%", "geom_w", "50%", "crop_l", "5%"); w.Code != 200 {
		t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
	}
	cue, err := ctp.GetCue("1")
	if err != nil {
		t.Fatal(err)
	}
	o := cueOpts(cue, false)
	if o.Opacity != 0.37 || o.GeomX != "10%" || o.GeomW != "50%" || o.CropL != "5%" {
		t.Fatalf("cueOpts: opacity %v geom %q/%q crop %q", o.Opacity, o.GeomX, o.GeomW, o.CropL)
	}
	sheet, err := ctp.GetCuesheet()
	if err != nil || len(sheet.Cues) != 1 || sheet.Cues[0].Opacity != 37 {
		t.Fatalf("cuesheet opacity: %+v %v", sheet.Cues, err)
	}
}

// A show carries Stop others, Layer and the "under" link; on an append
// import the link follows the cue to its new number.
func TestShowRoundTripKeepsLayers(t *testing.T) {
	r := setupTestServer(t)
	wav := buildTinyWav(1)
	for _, n := range []string{"la.wav", "lb.wav"} {
		if err := os.WriteFile(filepath.Join(config.MediaLocation(), n), wav, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ctp.RegisterMedia(n, int64(len(wav)), media.Metadata{Mimetype: "audio/wav", Duration: 1}, n); err != nil {
			t.Fatal(err)
		}
		if err := ctp.AddCue(n, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctp.UpdateCueFields("2", map[string]string{"stop_others": "false", "layer": "under", "layer_under": "1"}); err != nil {
		t.Fatal(err)
	}
	exp := get(t, r, "/api/show/export")
	if exp.Code != 200 {
		t.Fatalf("export = %d", exp.Code)
	}
	data := exp.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var m showManifest
	for _, f := range zr.File {
		if f.Name == "cutepi.json" {
			rc, _ := f.Open()
			json.NewDecoder(rc).Decode(&m)
			rc.Close()
		}
	}
	if len(m.Cues) != 2 || m.Cues[1].StopOthers == nil || *m.Cues[1].StopOthers || m.Cues[1].Layer != "under" || m.Cues[1].LayerUnder != m.Cues[0].CueNum {
		t.Fatalf("manifest: %+v", m.Cues)
	}

	// Append onto a sheet that already holds cue numbers 1 and 2.
	r2 := setupTestServer(t)
	layerSheet(t, 2)
	if w := postCTP(t, r2, data, "append"); w.Code != http.StatusSeeOther && w.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", w.Code, w.Body.String())
	}
	a, _ := ctp.GetCue("3")
	b, _ := ctp.GetCue("4")
	if a.Title != "la.wav" || b.Title != "lb.wav" {
		t.Fatalf("imported cues: %q, %q", a.Title, b.Title)
	}
	if b.StopOthers || b.Layer != "under" || b.LayerUnder != a.Cue_id || b.LayerUnderNum != a.CueNum {
		t.Fatalf("imported layer settings: %+v (want under cue %s)", b, a.CueNum)
	}
	if !a.StopOthers || a.Layer != "top" {
		t.Fatalf("imported defaults: %+v", a)
	}

	// An older show without the fields: Stop others on, top.
	old := ctp.ExportCue{Title: "x"}
	if raw, _ := json.Marshal(old); strings.Contains(string(raw), "stopOthers") || strings.Contains(string(raw), "layer") {
		t.Fatalf("empty layer fields not omitted: %s", raw)
	}
}

// Two cues fired through the API both run when the second keeps the others;
// a cue with Stop others on ends them all (DESIGN §6.1.2).
func TestFireKeepsOthersRunning(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available; cannot build an image fixture")
	}
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available")
	}
	t.Setenv("CUTEPI_WALL_SINK", "fakesink")
	r := setupTestServer(t)
	for i, c := range []string{"0x1a3a5c", "0x5c3a1a", "0x3a5c1a"} {
		name := "stack" + string(rune('1'+i)) + ".png"
		path := filepath.Join(config.MediaLocation(), name)
		if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c="+c+":size=320x240",
			"-frames:v", "1", path).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg fixture: %v %s", err, out)
		}
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "image/png", Resolution: "320x240", Codec: "png"}, name); err != nil {
			t.Fatal(err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	defer gsp.Stop()
	if err := ctp.UpdateCueFields("2", map[string]string{"stop_others": "false", "layer": "bottom"}); err != nil {
		t.Fatal(err)
	}
	running := func() []int {
		r := gsp.RunningCues()
		sort.Ints(r)
		return r
	}
	for _, pos := range []string{"1", "2"} {
		if w := post(t, r, "/api/cue/"+pos+"/play"); w.Code != http.StatusOK {
			t.Fatalf("play %s = %d: %s", pos, w.Code, w.Body.String())
		}
	}
	waitRunning := func(want string) {
		t.Helper()
		var got []int
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if got = running(); fmt.Sprint(got) == "["+want+"]" {
				return
			}
		}
		t.Fatalf("running = %v, want [%s]", got, want)
	}
	waitRunning("1 2")
	if gsp.CurrentCuePos() != 2 {
		t.Fatalf("focus = %d, want the newest cue (2)", gsp.CurrentCuePos())
	}

	// The Active Cues pane lists both and stops one alone.
	pane := get(t, r, "/api/activecues").Body.String()
	for _, want := range []string{`data-count="2"`, `/api/activecues/1/stop`, `/api/activecues/2/fade`, `stack1.png`, `stack2.png`} {
		if !strings.Contains(pane, want) {
			t.Fatalf("pane missing %s:\n%s", want, pane)
		}
	}
	if w := post(t, r, "/api/activecues/1/stop"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `data-count="1"`) {
		t.Fatalf("stop cue 1 alone = %d: %s", w.Code, w.Body.String())
	}
	waitRunning("2")
	if w := post(t, r, "/api/activecues/1/stop"); w.Code != http.StatusNotFound {
		t.Fatalf("stopping a cue that is not running = %d, want 404", w.Code)
	}
	if w := post(t, r, "/api/activecues/x/fade"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad position = %d, want 400", w.Code)
	}
	if body := get(t, r, "/").Body.String(); !strings.Contains(body, `id="activecues-pane"`) || !strings.Contains(body, `id="activecues-toggle"`) {
		t.Fatal("the page has no Active Cues pane or toggle")
	}
	// Cue 1 stops the others again; cue 2 joins it.
	post(t, r, "/api/cue/1/play")
	waitRunning("1")
	post(t, r, "/api/cue/2/play")
	waitRunning("1 2")
	// Cue 3 keeps the default: everything else stops.
	if w := post(t, r, "/api/cue/3/play"); w.Code != http.StatusOK {
		t.Fatalf("play 3 = %d: %s", w.Code, w.Body.String())
	}
	waitRunning("3")
}
