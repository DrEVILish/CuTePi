package routes

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"html/template"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/media"
)

// setupTestServer wires up a real gin engine against the real templates
// (parsed the same way main.go does) and an isolated in-memory DB, so these
// tests exercise the exact same rendering path a browser would hit. Several
// real bugs (an empty cuesheet on every render, a garbage value in the
// inline-edit form) only ever showed up at this level - go vet and
// package-level unit tests can't catch a template referencing the wrong
// field name.
func setupTestServer(t *testing.T) *gin.Engine {
	t.Helper()
	dir := t.TempDir()
	config.SetDbLocation(":memory:")
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	if err := ctp.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(AuthMiddleware()) // mirrors main.go; no-op unless a password is set
	r.Use(LimitBody())      // mirrors main.go
	// Same function map as the server (TemplateFuncs) — the two must never
	// drift apart or templates parse in one and panic in the other.
	r.SetHTMLTemplate(template.Must(template.New("").Funcs(TemplateFuncs()).ParseGlob("../templates/*")))

	Index(r.Group("/"))
	Api(r.Group("/api"))
	Groups(r.Group("/api"))
	Show(r.Group("/api"))
	Logs(r.Group("/api"))
	Upload(r.Group("/upload"))
	Youtube(r.Group("/youtube"))
	return r
}

func get(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func post(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
	return w
}

func postForm(t *testing.T, r *gin.Engine, path string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	return formRequest(t, r, "POST", path, kv)
}

func putForm(t *testing.T, r *gin.Engine, path string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	return formRequest(t, r, "PUT", path, kv)
}

func formRequest(t *testing.T, r *gin.Engine, method, path string, kv []string) *httptest.ResponseRecorder {
	t.Helper()
	if len(kv)%2 != 0 {
		t.Fatalf("formRequest: odd key/value pairs")
	}
	form := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(w, req)
	return w
}

func del(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("DELETE", path, nil))
	return w
}

func postJSON(t *testing.T, r *gin.Engine, path string, payload string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// Regression test for the bug found during live manual testing:
// cuesheet.html did `{{ range .Cuesheet }}` instead of
// `{{ range .Cuesheet.Cues }}`, which made html/template raise "range
// can't iterate over ..." on every single render - including the very
// first page load - silently truncating the response after gin had
// already flushed a 200 status and partial body.
func TestIndexRendersWithoutTemplateError(t *testing.T) {
	r := setupTestServer(t)
	w := get(t, r, "/")

	if w.Code != 200 {
		t.Fatalf("GET / = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "ERROR") {
		t.Fatalf("index page rendered an error page:\n%s", body)
	}
	if !strings.Contains(body, "</table>") && !strings.Contains(body, "Drag a cue from the Mediapool here.") {
		t.Fatalf("expected either the cuesheet table or empty-state guidance, got:\n%s", body)
	}
}

func TestYoutubeModalHasWorkingSubmitAndFeedback(t *testing.T) {
	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		`id="youtube-dl"`,
		`name="url"`,
		`required`,
		`type="submit"`,
		`id="ytdl-status"`,
		`id="ytdl-progress"`,
		`id="ytdl-rename"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected YouTube modal to contain %q, got:\n%s", want, body)
		}
	}
}

func TestYoutubeRejectsMissingURL(t *testing.T) {
	r := setupTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/youtube", strings.NewReader("url="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST /youtube without URL = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no URL provided") {
		t.Fatalf("expected missing URL error, got: %s", w.Body.String())
	}
}

// Renaming a downloaded file: on-disk rename, pool/cuesheet reconciliation,
// and a refreshed mediapool partial with the new name.
func TestYoutubeRename(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("ytd-old.mp4", 42, media.Metadata{Mimetype: "video/mp4"}, "ytd-old"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "ytd-old.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	form := url.Values{"old": {"ytd-old.mp4"}, "name": {"renamed clip"}}
	req := httptest.NewRequest(http.MethodPost, "/youtube/rename", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200: %s", w.Code, w.Body.String())
	}
	// Extension preserved: "renamed clip" -> "renamed clip.mp4".
	if !strings.Contains(w.Body.String(), `data-media-name="renamed clip.mp4"`) {
		t.Fatalf("mediapool partial missing renamed file:\n%s", w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(config.MediaLocation(), "renamed clip.mp4")); err != nil {
		t.Fatalf("file not renamed on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(config.MediaLocation(), "ytd-old.mp4")); err == nil {
		t.Fatal("old file still present after rename")
	}
}

func TestLogViewerEndpoints(t *testing.T) {
	r := setupTestServer(t)
	prev := logs.CurrentLevel()
	defer logs.SetLevel(prev)

	// The index page ships the modal and its topbar button.
	index := get(t, r, "/").Body.String()
	for _, want := range []string{`id="logsModal"`, `id="logs-feed"`, `data-bs-target="#logsModal"`} {
		if !strings.Contains(index, want) {
			t.Errorf("index page missing %q for the log viewer", want)
		}
	}

	// GET /api/logs returns the current level and buffered entries as JSON.
	logs := get(t, r, "/api/logs")
	if logs.Code != http.StatusOK {
		t.Fatalf("GET /api/logs = %d, want 200", logs.Code)
	}
	var body struct {
		Level   string `json:"level"`
		Entries []any  `json:"entries"`
	}
	if err := json.Unmarshal(logs.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET /api/logs is not JSON: %v", err)
	}
	if body.Level != "info" {
		t.Fatalf("default log level = %q, want info", body.Level)
	}

	// POST /api/logs/level switches the recording level.
	req := httptest.NewRequest(http.MethodPost, "/api/logs/level", strings.NewReader("level=warn"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/logs/level = %d, want 200", w.Code)
	}
	if lvl := get(t, r, "/api/logs"); !strings.Contains(lvl.Body.String(), `"level":"warn"`) {
		t.Fatalf("level not persisted after switch: %s", lvl.Body.String())
	}

	// DELETE /api/logs clears the viewer buffer.
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodDelete, "/api/logs", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("DELETE /api/logs = %d, want 200", w2.Code)
	}
}

func TestFeatureEndpointsRender(t *testing.T) {
	r := setupTestServer(t)

	qr := get(t, r, "/api/qr")
	if qr.Code != http.StatusOK || !strings.HasPrefix(qr.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("GET /api/qr = %d %q, want PNG", qr.Code, qr.Header().Get("Content-Type"))
	}
	if len(qr.Body.Bytes()) == 0 {
		t.Fatal("GET /api/qr returned an empty image")
	}

	inspector := get(t, r, "/api/cue/inspector")
	if inspector.Code != http.StatusOK || !strings.Contains(inspector.Body.String(), "Select a cue") {
		t.Fatalf("GET /api/cue/inspector did not render the empty state: %s", inspector.Body.String())
	}
}

func TestThemeLogoAndMobileUploadMarkup(t *testing.T) {
	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		`src="/img/cutepi-logo.svg"`,
		`id="settingsTheme"`,
		`value="lcars"`,
		`value="qlab"`,
		`value="blue-future"`,
		`hx-swap="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected control centre markup %q, got:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="retro"`) || strings.Contains(body, "80s Retro") {
		t.Fatalf("the 80s style must be limited to the logo, got:\n%s", body)
	}
	upload := get(t, r, "/upload/")
	if upload.Code != http.StatusOK || !strings.Contains(upload.Body.String(), `for="file-input"`) || !strings.Contains(upload.Body.String(), `/src/dropzone.js`) {
		t.Fatalf("standalone upload page is missing picker wiring: %d\n%s", upload.Code, upload.Body.String())
	}
}

func TestCuesheetRendersAddedCue(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("routes-test.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "routes-test.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("routes-test.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	w := post(t, r, "/api/cue/1")
	if w.Code != 200 {
		t.Fatalf("POST /api/cue/1 = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "routes-test.mp4") {
		t.Fatalf("expected the rendered cuesheet to contain the cue's title, got:\n%s", body)
	}
	if !strings.Contains(body, `class="cue table-warning"`) {
		t.Fatalf("expected the selected cue to be highlighted, got:\n%s", body)
	}
	if !strings.Contains(body, "00:00:10.000") {
		t.Fatalf("expected the cue duration to use the probed media duration, got:\n%s", body)
	}
}

func TestCueSelectionAdvancesSyncVersion(t *testing.T) {
	r := setupTestServer(t)
	for _, name := range []string{"sync-a.mp4", "sync-b.mp4"} {
		if err := ctp.RegisterMedia(name, 100, media.Metadata{
			Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
		}, name); err != nil {
			t.Fatalf("RegisterMedia(%q): %v", name, err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatalf("AddCue(%q): %v", name, err)
		}
	}
	before := ctp.CuesheetVersion()
	if w := post(t, r, "/api/cue/2"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/2 = %d: %s", w.Code, w.Body.String())
	}
	status := get(t, r, "/api/cuesheet/status?version="+strconv.FormatUint(before, 10))
	var body struct {
		Changed bool   `json:"changed"`
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode selection status: %v", err)
	}
	if !body.Changed || body.Version <= before {
		t.Fatalf("selection did not advance sync status: before=%d response=%s", before, status.Body.String())
	}
}

func TestAddCueRouteAllowsRepeatedMedia(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("repeat-route.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "repeat-route.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	for _, path := range []string{
		"/api/cue/add/repeat-route.mp4",
		"/api/cue/add/repeat-route.mp4/1",
	} {
		w := post(t, r, path)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s = %d, want 200: %s", path, w.Code, w.Body.String())
		}
	}
	body := get(t, r, "/api/cuesheet").Body.String()
	if !strings.Contains(body, "repeat-route.mp4 (2)") {
		t.Fatalf("expected repeated media to get a unique cue title, got:\n%s", body)
	}
}

func TestAddCueRejectsUnknownMedia(t *testing.T) {
	r := setupTestServer(t)
	w := post(t, r, "/api/cue/add/missing-route.mp4")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("POST missing cue = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), `id="cuesheet"`) {
		t.Fatalf("unknown media must not return a cuesheet partial: %s", w.Body.String())
	}
}

func TestCueActionsDoNotBubbleIntoRowSelection(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("action-route.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "action-route.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("action-route.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	body := get(t, r, "/").Body.String()
	// Cue actions live in the context menu and inspector only; clicking a row
	// must solely select it, so no per-row delete/play buttons are rendered.
	if strings.Contains(body, `data-cue-delete=`) || strings.Contains(body, `title="Delete cue"`) {
		t.Fatalf("cue-row action buttons should be removed, got:\n%s", body)
	}
	if !strings.Contains(body, `data-cue-pos="1"`) {
		t.Fatalf("expected the cue row with its selectable data attr, got:\n%s", body)
	}
}

func TestGroupRoutesCreateAssignUpdateDelete(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("group-route.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "group-route.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("group-route.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	// Add a group via the route.
	w := postForm(t, r, "/api/group/add", "name", "Route Group", "parentGroupID", "0")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/group/add = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cue-group-header") {
		t.Fatalf("expected group header row, got:\n%s", w.Body.String())
	}

	// Inspector renders slideshow settings for the group.
	g, err := ctp.GetGroup(1)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	w = get(t, r, "/api/group/1/inspector")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="slideshow"`) ||
		!strings.Contains(w.Body.String(), g.Name) {
		t.Fatalf("inspector missing settings, got: %d\n%s", w.Code, w.Body.String())
	}

	// Assign the cue to the group (moves it under the header).
	w = postForm(t, r, "/api/cue/1/group", "groupId", "1")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/1/group = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `data-cue-group="1"`) {
		t.Fatalf("cue should render inside the group, got:\n%s", w.Body.String())
	}

	// Turn on slideshow settings.
	w = putForm(t, r, "/api/group/1", "name", "Route Group", "slideshow", "true", "shuffle", "true", "loop", "true", "fadeSecs", "5", "durationSecs", "30")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/group/1 = %d: %s", w.Code, w.Body.String())
	}
	g, err = ctp.GetGroup(1)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	// Seconds in, milliseconds out (the inspector form labels say "(s)").
	if !g.Slideshow || !g.Shuffle || !g.Loop || g.FadeMS != 5000 || g.DurationMS != 30000 {
		t.Fatalf("group settings not applied: %+v", g)
	}

	// Collapse hides member rows.
	w = post(t, r, "/api/group/1/collapse")
	if w.Code != http.StatusOK {
		t.Fatalf("POST collapse = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `data-cue-group="1"`) {
		t.Fatalf("collapsed group should hide members, got:\n%s", w.Body.String())
	}


	// Deleting the group releases the cue.
	w = del(t, r, "/api/group/1")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /api/group/1 = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "cue-group-header") {
		t.Fatalf("group should be gone, got:\n%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `data-cue-pos="1"`) {
		t.Fatalf("released cue should still render, got:\n%s", w.Body.String())
	}
}

func TestGroupJoinFirstPutsCueFirst(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("joinfirst.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "joinfirst.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := ctp.AddCue("joinfirst.mp4", ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	if w := postForm(t, r, "/api/group/add", "name", "Join Group", "parentGroupID", "0"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/group/add = %d: %s", w.Code, w.Body.String())
	}
	// Two members join normally (end of group).
	if w := postForm(t, r, "/api/cue/1/group", "groupId", "1"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/1/group = %d: %s", w.Code, w.Body.String())
	}
	if w := postForm(t, r, "/api/cue/2/group", "groupId", "1"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/2/group = %d: %s", w.Code, w.Body.String())
	}
	// Expanded-header drop (§5.4): first=1 makes cue 3 the FIRST member.
	if w := postForm(t, r, "/api/cue/3/group", "groupId", "1", "first", "1"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/3/group first=1 = %d: %s", w.Code, w.Body.String())
	}
	members, err := ctp.GroupCuePositions(1)
	if err != nil {
		t.Fatalf("GroupCuePositions: %v", err)
	}
	if len(members) != 3 || members[0] != 3 {
		t.Fatalf("join-first should lead the group, got members %v", members)
	}
}

func TestGroupJoinLastKeepsOrder(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("joinlast.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "joinlast.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := ctp.AddCue("joinlast.mp4", ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	if w := postForm(t, r, "/api/group/add", "name", "Join Group", "parentGroupID", "0"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/group/add = %d: %s", w.Code, w.Body.String())
	}
	// Default join (collapsed highlight, §5.4) appends to the group's end.
	if w := postForm(t, r, "/api/cue/1/group", "groupId", "1"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/1/group = %d: %s", w.Code, w.Body.String())
	}
	if w := postForm(t, r, "/api/cue/2/group", "groupId", "1"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/2/group = %d: %s", w.Code, w.Body.String())
	}
	members, err := ctp.GroupCuePositions(1)
	if err != nil {
		t.Fatalf("GroupCuePositions: %v", err)
	}
	if len(members) != 2 || members[0] != 1 || members[1] != 2 {
		t.Fatalf("join-last should append in order, got members %v", members)
	}
}

func TestGroupJoinFirstInvalidGroup(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("joinbad.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "joinbad.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("joinbad.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if w := postForm(t, r, "/api/cue/1/group", "groupId", "999", "first", "1"); w.Code == http.StatusOK {
		t.Fatalf("join-first to a missing group should fail, got %d", w.Code)
	}
}

func TestSheetDropJoinFirstBlock(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("dropfirst.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "dropfirst.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := ctp.AddCue("dropfirst.mp4", ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	if w := postForm(t, r, "/api/group/add", "name", "Drop Group", "parentGroupID", "0"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/group/add = %d: %s", w.Code, w.Body.String())
	}
	if w := postForm(t, r, "/api/cue/1/group", "groupId", "1"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/1/group = %d: %s", w.Code, w.Body.String())
	}
	// /sheet/drop joinFirst moves a block to the head of the group.
	w := postJSON(t, r, "/api/sheet/drop", `{"cues":[2,3],"beforeKind":"group","beforeId":1,"join":true,"joinFirst":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/sheet/drop joinFirst = %d: %s", w.Code, w.Body.String())
	}
	members, err := ctp.GroupCuePositions(1)
	if err != nil {
		t.Fatalf("GroupCuePositions: %v", err)
	}
	if len(members) != 3 || members[0] != 2 || members[1] != 3 || members[2] != 1 {
		t.Fatalf("joinFirst block should lead the group in order, got %v", members)
	}
}

func TestBulkEditColorGroupDelete(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("bulkops.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "bulkops.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := ctp.AddCue("bulkops.mp4", ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	// Bulk colour applies to every position in one call.
	w := postJSON(t, r, "/api/cue/bulk", `{"op":"color","value":"#ff0000","positions":[1,2]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/bulk color = %d: %s", w.Code, w.Body.String())
	}
	for _, pos := range []int{1, 2} {
		cue, err := ctp.GetCue(strconv.Itoa(pos))
		if err != nil {
			t.Fatalf("GetCue %d: %v", pos, err)
		}
		if cue.Color != "#ff0000" {
			t.Fatalf("cue %d color = %q, want #ff0000", pos, cue.Color)
		}
	}
	// Bulk group assigns the whole selection (the context menu's Apply group
	// sends the picked group id as value).
	if w := postForm(t, r, "/api/group/add", "name", "Bulk Group", "parentGroupID", "0"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/group/add = %d: %s", w.Code, w.Body.String())
	}
	w = postJSON(t, r, "/api/cue/bulk", `{"op":"group","value":"1","positions":[1,2]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/bulk group = %d: %s", w.Code, w.Body.String())
	}
	members, err := ctp.GroupCuePositions(1)
	if err != nil {
		t.Fatalf("GroupCuePositions: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("bulk group should assign 2 members, got %v", members)
	}
	// Bulk group with an empty value (no target picked) must fail, not
	// silently assign group 0.
	w = postJSON(t, r, "/api/cue/bulk", `{"op":"group","value":"","positions":[1,2]}`)
	if w.Code == http.StatusOK {
		t.Fatalf("bulk group with empty value should fail, got %d", w.Code)
	}
	// Bulk delete removes the cues and the sheet re-renders.
	w = postJSON(t, r, "/api/cue/bulk", `{"op":"delete","value":"","positions":[1,2]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/bulk delete = %d: %s", w.Code, w.Body.String())
	}
	sheet, err := ctp.GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(sheet.Cues) != 1 {
		t.Fatalf("after bulk delete: %d cues, want 1", len(sheet.Cues))
	}
}

func TestGroupPlayNonSlideshowPlaysFirstMember(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("gplay-route.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "gplay-route.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if _, err := ctp.CreateGroup("Play Group", 0); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	cues, err := ctp.GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(cues.Cues) != 0 {
		t.Fatalf("cuesheet should be empty before adding, got %+v", cues.Cues)
	}
	if err := ctp.AddCue("gplay-route.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	cues, _ = ctp.GetCuesheet()
	if err := ctp.SetCueGroup(strconv.Itoa(cues.Cues[0].CuePos), 1); err != nil {
		t.Fatalf("SetCueGroup: %v", err)
	}
	w := post(t, r, "/api/group/1/play")
	if w.Code != http.StatusOK {
		t.Fatalf("POST play = %d: %s", w.Code, w.Body.String())
	}
}

func TestMediapoolClickDoesNotTriggerRefresh(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("dropdown-route.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "dropdown-route.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	body := get(t, r, "/").Body.String()
	if strings.Contains(body, `hx-get="/mediapool"`) {
		t.Fatalf("mediapool must not refresh on every click, got:\n%s", body)
	}
	if !strings.Contains(body, `data-media-menu="dropdown-route.mp4"`) {
		t.Fatalf("expected the three-dot menu button, got:\n%s", body)
	}
	// The menu button is a custom context menu toggle (not a Bootstrap
	// dropdown), so it carries data-media-menu and the media-menu-toggle
	// class.
	if !strings.Contains(body, `class="media-menu-toggle"`) {
		t.Fatalf("expected the media-menu-toggle button, got:\n%s", body)
	}
}

func TestMediapoolEscapesMediaActionPaths(t *testing.T) {
	r := setupTestServer(t)
	filename := "clip #1?.mp4"
	if err := ctp.RegisterMedia(filename, 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, filename); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	body := get(t, r, "/mediapool").Body.String()
	if !strings.Contains(body, `data-media-menu="`+filename+`"`) || !strings.Contains(body, `data-media-name="`+filename+`"`) {
		t.Fatalf("expected media filenames for client-side URL encoding, got:\n%s", body)
	}
}

// Column resizing is client-side, so the server can only guarantee the
// resize-handle markers are part of the rendered header: each resizable
// <th> carries data-column-resize and a .col-resize-handle span, which
// public/src/ui.js wires to localStorage-persisted drag-to-resize. The
// media-type and Cue No columns are content-fixed (not resizable).
func TestCuesheetRendersColumnResizeMarkers(t *testing.T) {
	r := setupTestServer(t)

	// The suite shares one in-memory DB: earlier tests leave cues behind and
	// a stale selection (e.g. the export/import roundtrip's SetCue("1"))
	// renders an inspector for THAT cue, resurrecting inline markup this
	// test asserts was removed. Start from a known, empty sheet (ClearCueSheet
	// also resets the selection; leftover cue_group rows are inert without
	// member cues).
	if err := ctp.ClearCueSheet(); err != nil {
		t.Fatalf("ClearCueSheet: %v", err)
	}

	if err := ctp.RegisterMedia("resize-route.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "resize-route.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("resize-route.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		`data-column-resize="cueName"`,
		`data-column-resize="preWait"`,
		`data-column-resize="cueDur"`,
		`data-column-resize="postWait"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the cuesheet header to contain %q, got:\n%s", want, body)
		}
	}
	// One handle per resizable column (4 total). The type + Cue No columns are
	// content-fixed and carry no resize handle.
	if got := strings.Count(body, `class="col-resize-handle"`); got != 4 {
		t.Fatalf("expected 4 column resize handles, got %d:\n%s", got, body)
	}
	// Media-type column: no label, content-fixed header class.
	if !strings.Contains(body, `class="col-type"`) || strings.Contains(body, `>Type<span`) {
		t.Fatalf("type column should be unlabelled and content-fixed, got:\n%s", body)
	}
	if !strings.Contains(body, `class="cue-type"`) {
		t.Fatalf("expected a media-type icon cell, got:\n%s", body)
	}
	// Cue No column: content-fixed, only the header title width.
	if !strings.Contains(body, `class="col-cue-num" title="Cue number">Number<`) {
		t.Fatalf("expected the content-fixed Number header, got:\n%s", body)
	}
	if strings.Contains(body, `data-column-resize="position"`) || strings.Contains(body, ">Position<") {
		t.Fatalf("position row-order column should not be displayed, got:\n%s", body)
	}
	if !strings.Contains(body, `class="cuesheet-blank-rows"`) {
		t.Fatalf("expected unused cuesheet space to keep the row rhythm, got:\n%s", body)
	}
	// The handle must not collide with the dblclick-to-edit cue cells, and no
	// per-row action buttons remain (editing is via the inspector/context menu).
	for _, want := range []string{`hx-trigger="dblclick"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected cuesheet edit markup %q to still render, got:\n%s", want, body)
		}
	}
	// Inline-edit cells must resolve their own swap target (the cell), not
	// inherit the row's #cuesheet target - otherwise the editor form would
	// replace the whole sheet. The row's select trigger is delayed and
	// cancellable (justEdited) so a single-click selection still works but
	// can't re-render the sheet out from under the editor opened by the
	// double-click that just happened.
	for _, want := range []string{`class="cue-inline-edit" hx-target="this" hx-trigger="dblclick"`, `click delay:250ms[!justEdited()]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected inline-edit markup %q to render, got:\n%s", want, body)
		}
	}
	for _, gone := range []string{`cue-loop-toggle`, `data-cue-delete="`, `hx-post="/api/cue/1/move/up"`, `hx-post="/api/cue/1/play"`} {
		if strings.Contains(body, gone) {
			t.Fatalf("cue-row action markup %q should be removed, got:\n%s", gone, body)
		}
	}
}

// Regression test for the bug found during live manual testing: the
// GET-equivalent handler for /api/cue/:cuePos/edit/:col passed the entire
// Cue struct as the edit form's value instead of the specific column being
// edited, so double-clicking a cell showed a stringified Go struct instead
// of the current value.
func TestCueEditFormPrefillsActualColumnValue(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("edit-test.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "edit-test.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("edit-test.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if err := ctp.UpdateCue("1", "title", "Distinctive Title"); err != nil {
		t.Fatalf("UpdateCue: %v", err)
	}

	w := post(t, r, "/api/cue/1/edit/title")
	if w.Code != 200 {
		t.Fatalf("POST /api/cue/1/edit/title = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `value="Distinctive Title"`) {
		t.Fatalf("expected the edit form to be pre-filled with the current title, got:\n%s", body)
	}
	if strings.Contains(body, "db:") || strings.Contains(body, "Media_id") {
		t.Fatalf("edit form leaked a stringified struct instead of the column value:\n%s", body)
	}
}

// Regression test: DELETE /api/cue/:cuePos must return the re-rendered
// cuesheet. The delete button replaces #cuesheet, so an empty 200 body would
// wipe the whole table.
func TestDeleteCueReturnsRenderedCuesheet(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("delroute.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "delroute.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("delroute.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/cue/1", nil))
	if w.Code != 200 {
		t.Fatalf("DELETE /api/cue/1 = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "<div id=\"cuesheet\">") {
		t.Fatalf("expected DELETE /api/cue/1 to return the rendered cuesheet, got:\n%s", body)
	}
	if strings.Contains(body, "delroute.mp4") {
		t.Fatalf("expected the deleted cue to be absent from the rendered cuesheet:\n%s", body)
	}
}

// Regression test: DELETE /api/media/:filename must return the re-rendered
// mediapool partial so the deleted tile is removed from the DOM (the delete
// button targets #mediapool). An empty 200 previously removed the wrong
// element (the dropdown <li>) or left a stale tile behind.
func TestDeleteMediaReturnsRenderedMediapool(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("delmedia.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "delmedia.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/media/delmedia.mp4", nil))
	if w.Code != 200 {
		t.Fatalf("DELETE /api/media/delmedia.mp4 = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "mediapool") {
		t.Fatalf("expected DELETE /api/media to return the mediapool partial, got:\n%s", body)
	}
	if strings.Contains(body, "delmedia.mp4") {
		t.Fatalf("expected the deleted media tile to be gone from the re-rendered pool:\n%s", body)
	}
}

// The standalone mediapool route remains available for explicit refreshes and
// post-upload rendering.
func TestMediapoolPartialRouteExists(t *testing.T) {
	r := setupTestServer(t)
	w := get(t, r, "/mediapool")
	if w.Code != 200 {
		t.Fatalf("GET /mediapool = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Mediapool") {
		t.Fatalf("expected GET /mediapool to render the mediapool partial, got:\n%s", w.Body.String())
	}
}

// The htmx v4 compat flags have been migrated out: the htmx-config meta must
// be gone, and attribute inheritance must be explicit (the cuesheet edit cells
// inherit hx-target from their <tr>, now expressed as hx-target:inherited).
func TestHtmxCompatFlagsRemoved(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("flags-removed.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "flags-removed.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("flags-removed.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	body := get(t, r, "/").Body.String()
	if strings.Contains(body, "htmx-config") {
		t.Fatalf("expected the htmx-config compat meta to be removed, got:\n%s", body)
	}
	// The non-selected cue row explicitly targets the #cuesheet swap target
	// (htmx v4 attribute), and its inversion cells inherit that target via the
	// v4 `hx-target:inherited` colon-modifier (the v2 config-flag hack is gone).
	if !strings.Contains(body, `hx-target="#cuesheet"`) {
		t.Fatalf("expected the cue row to carry hx-target='#cuesheet', got:\n%s", body)
	}
	if !strings.Contains(body, `hx-target:inherited="#cuesheet"`) {
		t.Fatalf("expected the cue row to carry hx-target:inherited='#cuesheet', got:\n%s", body)
	}
}

// The mediapool renders filter controls and per-tile data attributes used by
// the client-side filename/type filtering.
func TestMediapoolRendersFilterControlsAndType(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("filter-video.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "filter-video.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.RegisterMedia("filter-img.png", 100, media.Metadata{
		Mimetype: "image/png", Duration: 0, Resolution: "1280x720", Codec: "png",
	}, "filter-img.png"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}

	// Freshly registered media have a pending thumbnail (the background
	// worker hasn't run), so mediapoolView serves an empty thumbnail URL and
	// the template must fall back to the per-type SVG placeholder. Mark the
	// image item's thumbnail done so it renders its real thumbnail URL.
	imageMediaID := 0
	pool, err := ctp.GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	for _, m := range pool.Medias {
		if m.Filename == "filter-img.png" {
			imageMediaID = m.Media_id
		}
	}
	if imageMediaID == 0 {
		t.Fatalf("could not find filter-img.png in the media pool")
	}
	if err := ctp.MarkThumbnailDone(imageMediaID); err != nil {
		t.Fatalf("MarkThumbnailDone: %v", err)
	}

	body := get(t, r, "/mediapool").Body.String()
	if !strings.Contains(body, "mediaFilterText") || !strings.Contains(body, "mediaFilterType") {
		t.Fatalf("expected the mediapool to render filter controls, got:\n%s", body)
	}
	if !strings.Contains(body, `data-media-type="video"`) || !strings.Contains(body, `data-media-type="image"`) {
		t.Fatalf("expected tiles to carry a data-media-type attribute, got:\n%s", body)
	}
	if !strings.Contains(body, `data-media-name="filter-video.mp4"`) {
		t.Fatalf("expected tiles to carry a data-media-name attribute, got:\n%s", body)
	}
	if !strings.Contains(body, "url('/img/placeholder-video.svg')") {
		t.Fatalf("expected the pending video tile to render the video placeholder SVG, got:\n%s", body)
	}
	if strings.Contains(body, "placeholder-image.svg") {
		t.Fatalf("expected the completed image tile not to render a placeholder, got:\n%s", body)
	}
	if !strings.Contains(body, "url('/thumbnails/filter-img.png.jpg')") {
		t.Fatalf("expected the completed image tile to keep its thumbnail URL, got:\n%s", body)
	}
	// Filter-video.mp4 was registered just before this page rendered, so its
	// "date added" should appear formatted on the tile.
	if !regexp.MustCompile(`filter-video\.mp4[\s\S]{0,400}20\d\d-\d\d-\d\d \d\d:\d\d`).MatchString(body) {
		t.Fatalf("expected the mediapool tile to display the date added, got:\n%s", body)
	}
}

// The standalone /upload page renders (no longer a "pong" stub) and is
// mobile-friendly: it has the dropzone form and a link back to the control
// centre.
func TestUploadPageRenders(t *testing.T) {
	r := setupTestServer(t)
	w := get(t, r, "/upload/")
	if w.Code != 200 {
		t.Fatalf("GET /upload/ = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"dropform", "Choose a file", "enctype=\"multipart/form-data\"", "Control centre", `href="/"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the upload page to contain %q, got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "pong") {
		t.Fatalf("GET /upload/ should not return the old stub, got:\n%s", body)
	}
}

// Uploads without files are rejected with an error page, whether the request
// comes from the desktop modal (htmx) or the standalone page (plain POST).
func TestUploadRejectsNoFiles(t *testing.T) {
	r := setupTestServer(t)

	multipartBody := func() (io.Reader, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.Close()
		return &buf, w.FormDataContentType()
	}

	body, ct := multipartBody()
	req := httptest.NewRequest("POST", "/upload/", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("plain POST /upload/ with no files = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no files uploaded") {
		t.Fatalf("expected a no-files error, got:\n%s", w.Body.String())
	}

	body2, ct2 := multipartBody()
	req2 := httptest.NewRequest("POST", "/upload/", body2)
	req2.Header.Set("Content-Type", ct2)
	req2.Header.Set("HX-Request", "true")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("htmx POST /upload/ with no files = %d, want 400", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "no files uploaded") {
		t.Fatalf("expected a no-files error for htmx request, got:\n%s", w2.Body.String())
	}
}

// The index page carries the media-pool panel chrome (collapse button, the
// always-available expand button, and the drag-to-resize splitter).
func TestIndexRendersPanelChrome(t *testing.T) {
	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		`id="mediapool-pane"`,
		`id="mediapool-collapse-btn"`,
		`id="mediapool-expand-btn"`,
		`id="mediapool-resizer"`,
		`id="cuesheet-pane"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the index page to contain %q, got:\n%s", want, body)
		}
	}
}

// The "Show Test" modal is included on the index page with a "Hide Test"
// button that posts to /api/stop. The Test button posts to /api/test and
// opens the modal on success (hx-on:htmx:after-request), so this only asserts
// the modal markup and the htmx wiring render.
func TestIndexRendersTestModal(t *testing.T) {
	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		`id="testModal"`,
		`test-pattern-grid`,
		`hx-post="/api/stop"`,
		`Hide Test`,
		`id="showTestBtn"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the index page to contain %q, got:\n%s", want, body)
		}
	}
}

// An empty media pool renders an upload placeholder with a button and
// drag-and-drop help text instead of an empty grid.
func TestMediapoolEmptyState(t *testing.T) {
	r := setupTestServer(t)
	// No media registered -> empty pool.
	body := get(t, r, "/mediapool").Body.String()
	for _, want := range []string{
		"No media yet",
		`data-bs-target="#uploadModal"`,
		"Upload",
		"or drop files here",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the empty media pool to contain %q, got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "mediaFilterText") {
		t.Fatalf("expected no filter controls in the empty state, got:\n%s", body)
	}
}

// The empty-state placeholder disappears once media exists.
func TestMediapoolEmptyStateWithMedia(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("not-empty.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "not-empty.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	body := get(t, r, "/mediapool").Body.String()
	if strings.Contains(body, "No media yet") {
		t.Fatalf("expected no empty-state placeholder when media exists, got:\n%s", body)
	}
}

func TestLoadCueRouteExists(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("loadroute.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "loadroute.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	// The MediaPool "Load" dropdown action posts here; the handler loads via
	// gsp. In this test gsp is not initialized (no gstreamer), so it should
	// still hit the route and return an error/status rather than a gin 404
	// "no route" for a bogus path.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/load/loadroute.mp4", nil))
	if w.Code == http.StatusNotFound {
		t.Fatalf("expected /api/load/:filename route to exist, got 404")
	}
}

// The settings modal populates itself from GET /api/settings: 200 with the
// port and pollInterval JSON keys.
func TestSettingsGet(t *testing.T) {
	r := setupTestServer(t)

	w := get(t, r, "/api/settings")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected a JSON response, got: %s", w.Body.String())
	}
	for _, key := range []string{"port", "pollInterval"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("expected the settings response to contain %q, got: %s", key, w.Body.String())
		}
	}
}

// POST /api/settings persists the values and replies with the restart-needed
// message the Settings modal displays to the user.
func TestSettingsPost(t *testing.T) {
	r := setupTestServer(t)

	form := url.Values{
		"port":         {"4010"},
		"pollInterval": {"250"},
	}
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/settings = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected a JSON response, got: %s", w.Body.String())
	}
	if msg, ok := body["message"].(string); !ok || !strings.Contains(msg, "restart") {
		t.Fatalf("expected a restart-required message, got: %s", w.Body.String())
	}
}

// POST /api/settings accepts the loop default. It must round-trip through
// GET /api/settings. (Volume is per-cue, not a global setting.)
func TestSettingsLoop(t *testing.T) {
	r := setupTestServer(t)

	form := url.Values{
		"loop": {"true"},
	}
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/settings = %d, want 200: %s", w.Code, w.Body.String())
	}

	got := get(t, r, "/api/settings")
	if got.Code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d, want 200", got.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON, got: %s", got.Body.String())
	}
	if loop, ok := body["loop"].(bool); !ok || !loop {
		t.Fatalf("expected loop enabled after save, got: %v", body["loop"])
	}
	if _, present := body["volume"]; present {
		t.Fatalf("global default volume is gone; settings must not expose volume, got: %v", body["volume"])
	}
}

// The transport controls (seek endpooint, per-cue volume) must reach their
// handlers and return 200 with valid input, and 400 with bogus values. gsp is
// a no-op when nothing is loaded, so these are safe against the test process.
func TestTransportControlsEndpoints(t *testing.T) {
	r := setupTestServer(t)

	p := func(path, form string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := p("/api/seek", "position=12.5"); code != http.StatusOK {
		t.Fatalf("POST /api/seek = %d, want 200", code)
	}
	if code := p("/api/seek", "position=abc"); code != http.StatusBadRequest {
		t.Fatalf("POST /api/seek with garbage = %d, want 400", code)
	}
	if code := p("/api/volume", "volume=0.5"); code != http.StatusOK {
		t.Fatalf("POST /api/volume = %d, want 200", code)
	}
	if code := p("/api/volume", "volume=nan"); code != http.StatusBadRequest {
		t.Fatalf("POST /api/volume with garbage = %d, want 400", code)
	}
}

// systemdUnit resolves the unit name from the environment: only when running
// under systemd (INVOCATION_ID set), defaulting to cutepi but honoring a
// CUTEPI_SERVICE override. Outside a unit it is "", so standalone launches keep
// the self re-exec restart path.
func TestSystemdUnitResolution(t *testing.T) {
	setenv := func(kv map[string]string) {
		for k, v := range kv {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	}
	defer setenv(map[string]string{"INVOCATION_ID": "", "CUTEPI_SERVICE": ""})

	setenv(map[string]string{"INVOCATION_ID": "", "CUTEPI_SERVICE": "custom"})
	if got := systemdUnit(); got != "" {
		t.Fatalf("no INVOCATION_ID -> expected %q, got %q", "", got)
	}
	setenv(map[string]string{"INVOCATION_ID": "abc123", "CUTEPI_SERVICE": ""})
	if got := systemdUnit(); got != "cutepi" {
		t.Fatalf("under systemd with no override -> expected cutepi, got %q", got)
	}
	setenv(map[string]string{"INVOCATION_ID": "abc123", "CUTEPI_SERVICE": "cutepi-custom"})
	if got := systemdUnit(); got != "cutepi-custom" {
		t.Fatalf("CUTEPI_SERVICE override -> expected cutepi-custom, got %q", got)
	}
}

// Round-trip: POST /api/volume must apply the dB value to the active cue and
// persist the (clamped) dB onto its cuesheet row, so the setting survives a
// reload. gsp.SetCuePos simulates an active cue without a real pipeline.
func TestVolumeEndpointPersistsClampedDBToActiveCue(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("vol-rt.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1280x720",
	}, "vol-rt.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("vol-rt.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	sheet, err := ctp.GetCuesheet()
	if err != nil || len(sheet.Cues) == 0 {
		t.Fatalf("GetCuesheet: err=%v n=%d", err, len(sheet.Cues))
	}
	var pos int
	for _, cue := range sheet.Cues {
		if cue.Title == "vol-rt.mp4" {
			pos = cue.CuePos
		}
	}
	if pos == 0 {
		t.Fatalf("test cue not found in cuesheet")
	}
	gsp.SetCuePos(pos)

	postVol := func(v string) (*httptest.ResponseRecorder, float64) {
		req := httptest.NewRequest("POST", "/api/volume",
			strings.NewReader("volume="+v))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		cue, err := ctp.GetCue(strconv.Itoa(pos))
		if err != nil {
			t.Fatalf("GetCue(%d): %v", pos, err)
		}
		return w, cue.Volume
	}

	if w, vol := postVol("3"); w.Code != http.StatusOK || vol != 3 {
		t.Fatalf("volume=3 -> code=%d vol=%v, want 200, 3", w.Code, vol)
	}
	if w, vol := postVol("100"); w.Code != http.StatusOK || vol != 12 {
		t.Fatalf("volume=100 -> code=%d vol=%v, want 200, 12 (clamped)", w.Code, vol)
	}
	if w, vol := postVol("-100"); w.Code != http.StatusOK || vol != -60 {
		t.Fatalf("volume=-100 -> code=%d vol=%v, want 200, -60 (clamped)", w.Code, vol)
	}
}

// Selecting a cue via POST /api/cue/:cuePos must persist the selection to the
// state table (SelectedCuePos) and render the cuesheet. This is the path that
// surfaced as "attempt to write a readonly database" when a service held a
// stale/deleted DB, so it guards the write path end to end.
func TestSelectCuePersistsAndRenders(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("sel-rt.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1280x720",
	}, "sel-rt.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("sel-rt.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	sheet, err := ctp.GetCuesheet()
	if err != nil || len(sheet.Cues) == 0 {
		t.Fatalf("GetCuesheet: err=%v n=%d", err, len(sheet.Cues))
	}
	var pos int
	for _, cue := range sheet.Cues {
		if cue.Title == "sel-rt.mp4" {
			pos = cue.CuePos
		}
	}
	if pos == 0 {
		t.Fatalf("test cue not found in cuesheet")
	}

	req := httptest.NewRequest("POST", "/api/cue/"+strconv.Itoa(pos), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/cue/%d = %d, want 200", pos, w.Code)
	}
	if got, _ := ctp.SelectedCuePos(); got != pos {
		t.Fatalf("SelectedCuePos = %d, want %d (selection not persisted)", got, pos)
	}
	if !strings.Contains(w.Body.String(), "table-warning") {
		t.Fatalf("expected the selected cue to render as selected in the cuesheet")
	}
}

// The Now Playing widget's transport controls. The scrubber only renders once
// a clip is loaded; the inline script wiring it to the endpoint is always
// present in the partial. Loop and volume are per-cue (inspector/context
// menu), so they are not Now Playing controls.
func TestNowPlayingWidgetControlsMarkup(t *testing.T) {
	r := setupTestServer(t)
	body := get(t, r, "/api/nowplaying").Body.String()
	// Endpoint wiring is unconditional.
	for _, want := range []string{`/api/seek`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected Now Playing script to reference %q, got:\n%s", want, body)
		}
	}
	// Controls appear once something is loaded.
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "control-tone.wav"), buildTinyWav(10), 0o644); err != nil {
		t.Fatal(err)
	}
	defer gsp.Panic()
	mediaProbe := media.Metadata{Mimetype: "audio/wav", Duration: 10, Kind: media.KindAudio, Codec: "pcm_s16le"}
	if err := ctp.RegisterMedia("control-tone.wav", 40000, mediaProbe, "Control Tone"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := gsp.Load("control-tone.wav"); err != nil {
		t.Fatalf("gsp.Load: %v", err)
	}
	body = get(t, r, "/api/nowplaying").Body.String()
	for _, want := range []string{`id="nowplaying-scrubber"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected loaded Now Playing markup to contain %q, got:\n%s", want, body)
		}
	}
	// Loop/volume are per-cue controls, not Now Playing controls.
	for _, gone := range []string{`id="nowplaying-volume"`, `id="nowplaying-loop"`} {
		if strings.Contains(body, gone) {
			t.Fatalf("Now Playing should not render a %s control, got:\n%s", gone, body)
		}
	}
}

// The topbar Restart/Shutdown actions must reach their handlers and return
// 200 with the hooks swapped for harmless stand-ins (the real ones kill the
// process), so the test process survives.
func TestRestartAndShutdownEndpoints(t *testing.T) {
	oldRestart, oldShutdown := restartServer, shutdownServer
	defer func() { restartServer, shutdownServer = oldRestart, oldShutdown }()
	var restarted, shutdowned bool
	restartServer = func(time.Duration) error { restarted = true; return nil }
	shutdownServer = func(time.Duration) { shutdowned = true }

	r := setupTestServer(t)
	if w := post(t, r, "/api/restart"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/restart = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !restarted {
		t.Fatal("POST /api/restart did not invoke the restart hook")
	}
	if w := post(t, r, "/api/shutdown"); w.Code != http.StatusOK {
		t.Fatalf("POST /api/shutdown = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !shutdowned {
		t.Fatal("POST /api/shutdown did not invoke the shutdown hook")
	}

	// The markup advertises both actions with a native htmx confirm.
	body := get(t, r, "/").Body.String()
	for _, want := range []string{`hx-post="/api/restart"`, `hx-post="/api/shutdown"`, `hx-confirm=`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected topbar Restart/Shutdown markup %q, got:\n%s", want, body)
		}
	}
}

func TestClearCuesheetRendersEmptyTable(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("clear-test.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "clear-test.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("clear-test.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	w := post(t, r, "/api/clear")
	if w.Code != 200 {
		t.Fatalf("POST /api/clear = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "clear-test.mp4") {
		t.Fatalf("expected the cuesheet to be empty after /api/clear, got:\n%s", w.Body.String())
	}
}

// GET /api/nowplaying must keep returning the rendered mediainfo.html partial
// for the initial page render and for the change-detection poller's re-render
// path.
func TestNowPlayingHTMLRenders(t *testing.T) {
	r := setupTestServer(t)

	w := get(t, r, "/api/nowplaying")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/nowplaying = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="mediainfo"`) {
		t.Fatalf("expected the rendered mediainfo widget, got:\n%s", body)
	}
	if !strings.Contains(body, "header-status") {
		t.Fatalf("expected the Now Playing widget markup, got:\n%s", body)
	}
}

// The lightweight change-detection endpoint must return 200 with the
// {"changed", "version"} shape, computing changed against the version the
// client last saw (passed as ?version=, no per-client server state). The gsp
// change counter is a shared package global, so assertions are relative to the
// version current at request time.
func TestNowPlayingStatus(t *testing.T) {
	r := setupTestServer(t)

	w := get(t, r, "/api/nowplaying/status")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/nowplaying/status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected a JSON response, got: %s", w.Body.String())
	}
	for _, key := range []string{"changed", "version"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("expected the status response to contain %q, got: %s", key, w.Body.String())
		}
	}
	version := body["version"].(string)

	// A client that last saw exactly the current version has nothing to
	// re-render; one that last saw a different version does. The version is
	// a string pair ("<gsp>.<cuesheet>") — selection changes must mark the
	// header partial stale too.
	var ubody struct {
		Changed bool   `json:"changed"`
		Version string `json:"version"`
	}
	unchanged := get(t, r, "/api/nowplaying/status?version="+version)
	if err := json.Unmarshal(unchanged.Body.Bytes(), &ubody); err != nil {
		t.Fatalf("expected a JSON response, got: %s", unchanged.Body.String())
	}
	if ubody.Changed {
		t.Fatalf("expected changed=false for a client at the current version, got: %s", unchanged.Body.String())
	}

	var sbody struct {
		Changed bool   `json:"changed"`
		Version string `json:"version"`
	}
	stale := get(t, r, "/api/nowplaying/status?version="+version+".1")
	if err := json.Unmarshal(stale.Body.Bytes(), &sbody); err != nil {
		t.Fatalf("expected a JSON response, got: %s", stale.Body.String())
	}
	if !sbody.Changed {
		t.Fatalf("expected changed=true for a stale client, got: %s", stale.Body.String())
	}
}

// The Cue Inspector renders for a selected cue (trim timeline branch + the
// per-cue play-option fields) without a template error, even when the media
// duration is a float (seconds). Regression for the "incompatible types for
// comparison: float64 and int" template error.
func TestInspectorRendersForSelectedCue(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("insp-sel.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 12.5, Resolution: "1920x1080", Codec: "h264",
	}, "insp-sel.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("insp-sel.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	// Select via the same POST a row click issues, so this exercises the
	// selection -> inspector wiring (a click selects, then the client
	// re-fetches the inspector).
	if w := post(t, r, "/api/cue/1"); w.Code != 200 {
		t.Fatalf("POST /api/cue/1 = %d, want 200: %s", w.Code, w.Body.String())
	}

	w := get(t, r, "/api/cue/inspector")
	if w.Code != 200 {
		t.Fatalf("GET /api/cue/inspector = %d, want 200", w.Code)
	}
	// The inspector is selection-dependent, so it must never be served from a
	// cache (a cached response would show a previously selected cue).
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("GET /api/cue/inspector Cache-Control = %q, want no-store", cc)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="trim-timeline"`,
		`name="loop"`,
		`name="autoContinue"`,
		`name="color"`,
		`name="fadeAction"`,
		`name="fadeOut"`,
		`id="insp-hold"`,
		`name="volume"`,
		`name="rate"`,
		`name="balance"`,
		`name="mute"`,
		`data-tabpane="video"`,
		`id="insp-video-fadein"`,
		`id="insp-audio-fadein"`,
		`data-volume-reset`,
		`data-volume-step="-1"`,
		`data-volume-step="1"`,
		`Source: insp-sel.mp4`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected inspector markup %q, got:\n%s", want, body)
		}
	}
	// The inspector colour control is a 12-swatch chip picker, not a native
	// colour picker.
	if strings.Contains(body, `type="color"`) {
		t.Fatalf("inspector must not use a colour picker input, got:\n%s", body)
	}
	// The fixed 12-named palette renders as swatch radios named "color".
	if got := strings.Count(body, `name="color"`); got < 12 {
		t.Fatalf("expected >= 12 colour swatch radios, got %d:\n%s", got, body)
	}
	// Volume is a vertical dB slider (range -60..12, 0 = 0dB), not a plain input.
	if !strings.Contains(body, `name="volume" type="range" min="-60" max="12" step="1"`) {
		t.Fatalf("expected a vertical dB volume range slider, got:\n%s", body)
	}
	if !strings.Contains(body, `id="insp-volume-db"`) {
		t.Fatalf("expected the dB readout next to the volume slider, got:\n%s", body)
	}
	if !strings.Contains(body, "incompatible types") && !strings.Contains(body, "error calling") {
		return
	}
	t.Fatalf("inspector rendered a template error:\n%s", body)
}

// An image cue has no audio stream: the inspector must not offer an Audio tab
// pane or the "playback duration unknown" nag (a static image has no
// duration), and should say so instead.
func TestInspectorHidesAudioForImageCue(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("insp-img.png", 100, media.Metadata{
		Mimetype: "image/png", Duration: 0, Resolution: "800x600",
	}, "insp-img.png"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("insp-img.png", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if w := post(t, r, "/api/cue/1"); w.Code != 200 {
		t.Fatalf("POST /api/cue/1 = %d, want 200: %s", w.Code, w.Body.String())
	}

	body := get(t, r, "/api/cue/inspector").Body.String()
	// Images get display timing + frame geometry (video pane) but no audio
	// pane and no trim timeline.
	for _, want := range []string{`data-no-audio-tab`, `data-tabpane="video"`, `name="cueDuration"`, `name="fit_mode"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected inspector markup %q, got:\n%s", want, body)
		}
	}
	for _, forbid := range []string{`data-tabpane="audio"`, "no playback timing or trim", "Playback duration unknown"} {
		if strings.Contains(body, forbid) {
			t.Fatalf("unexpected inspector markup %q, got:\n%s", forbid, body)
		}
	}
}

// The Media tab re-probes and persists fresh audio info when a file was
// imported before audio metadata was collected (stale media_meta) - the same
// repair a ".webm has no audio info" report hits. A real file is written to
// disk so media.Probe succeeds with real ffprobe.
func TestInspectorSelfHealsStaleAudioMeta(t *testing.T) {
	r := setupTestServer(t)
	dir := config.MediaLocation()
	wav := buildTinyWav(1)
	if err := os.WriteFile(filepath.Join(dir, "self-heal.wav"), wav, 0o644); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	// Old import: base columns but no media_meta JSON (audio-info support
	// arrived in a later build than this file's import).
	if err := ctp.RegisterMedia("self-heal.wav", int64(len(wav)), media.Metadata{
		Mimetype: "audio/wav", Duration: 1.0, Codec: "pcm_s16le", Kind: media.KindAudio,
	}, "self-heal.wav"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("self-heal.wav", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	if w := post(t, r, "/api/cue/1"); w.Code != 200 {
		t.Fatalf("POST /api/cue/1 = %d, want 200", w.Code)
	}
	body := get(t, r, "/api/cue/inspector").Body.String()
	if !strings.Contains(body, "pcm_s16le") {
		t.Fatalf("Media tab did not self-heal audio info from the re-probe:\n%s", body)
	}
	// The repair must be persisted, not just painted once.
	pool, err := ctp.GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	for _, m := range pool.Medias {
		if m.Filename == "self-heal.wav" && strings.Contains(m.MediaInfo, "pcm_s16le") {
			return
		}
	}
	t.Fatalf("media_meta was not persisted after the self-heal re-probe: %+v", pool.Medias)
}

// The per-cue playback columns (loop, colour, autofollow, fade) round-trip
// through the API and are reflected in the rendered cuesheet/context-menu
// attributes, and the /api/fade endpoint is wired.
func TestCuePlaybackColumnsAndFadeAPI(t *testing.T) {
	r := setupTestServer(t)

	if err := ctp.RegisterMedia("cols-route.mp4", 100, media.Metadata{
		Mimetype: "video/mp4", Duration: 10, Resolution: "1920x1080", Codec: "h264",
	}, "cols-route.mp4"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("cols-route.mp4", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	body := get(t, r, "/api/cuesheet").Body.String()
	if !strings.Contains(body, `class="col-type"`) {
		t.Fatalf("expected the media-type column header, got:\n%s", body)
	}
	if !strings.Contains(body, `class="cue-type"`) {
		t.Fatalf("expected a media-type cell, got:\n%s", body)
	}
	if !strings.Contains(body, `data-cue-pos="1"`) {
		t.Fatalf("expected a cue row with its selectable data attr, got:\n%s", body)
	}

	// Loop is per-cue and edited from the inspector, not the row, so the row
	// must not carry a loop toggle/attribute.
	if strings.Contains(body, `data-cue-loop=`) {
		t.Fatalf("row must not expose a loop attribute (inspector-only), got:\n%s", body)
	}

	// PUT loop=true and re-check the inspector's Loop checkbox reflects it.
	req := httptest.NewRequest("PUT", "/api/cue/1/edit/loop", bytes.NewBufferString("col=loop&val=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT loop = %d, want 200", w.Code)
	}
	cue, err := ctp.GetCue("1")
	if err != nil {
		t.Fatalf("GetCue: %v", err)
	}
	if !cue.Loop {
		t.Fatalf("expected cue.Loop=true after update, got false")
	}

	// PUT a colour and check the row accent style appears.
	req2 := httptest.NewRequest("PUT", "/api/cue/1/edit/color", bytes.NewBufferString("col=color&val=#ff0055"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("PUT color = %d, want 200", w2.Code)
	}
	body = get(t, r, "/api/cuesheet").Body.String()
	if !strings.Contains(body, "#ff0055") {
		t.Fatalf("expected the cue colour in the rendered row, got:\n%s", body)
	}

	// /api/fade is wired (no active pipeline -> 200 no-op).
	if w3 := post(t, r, "/api/fade"); w3.Code != 200 {
		t.Fatalf("POST /api/fade = %d, want 200", w3.Code)
	}
}

// TestShowExportImportRoundtrip is the runnable check for .CTP export/import:
// a show (media + cues + settings + selection) exported from one server
// restores identically on a fresh one via append, then via overwrite.
func TestShowExportImportRoundtrip(t *testing.T) {
	r := setupTestServer(t)

	// Register a real (tiny WAV) media file on disk so the export embeds it.
	wav := buildTinyWav(1.0)
	wavPath := filepath.Join(config.MediaLocation(), "roundtrip.wav")
	if err := os.WriteFile(wavPath, wav, 0o644); err != nil {
		t.Fatalf("writing media fixture: %v", err)
	}
	if err := ctp.RegisterMedia("roundtrip.wav", int64(len(wav)), media.Metadata{
		Mimetype: "audio/wav", Duration: 1.0, Codec: "pcm_s16le",
	}, "roundtrip.wav"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("roundtrip.wav", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}
	// Give the cue settings worth preserving.
	if err := ctp.UpdateCue("1", "color", "#ff0055"); err != nil {
		t.Fatalf("UpdateCue(color): %v", err)
	}
	_ = ctp.SetCue("1")

	// Export.
	exp := get(t, r, "/api/show/export")
	if exp.Code != 200 {
		t.Fatalf("GET /api/show/export = %d, want 200: %s", exp.Code, exp.Body.String())
	}
	data := exp.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("export is not a zip: %v", err)
	}
	var manifest showManifest
	foundMedia := false
	for _, f := range zr.File {
		switch f.Name {
		case "cutepi.json":
			rc, _ := f.Open()
			if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
				t.Fatalf("decoding cutepi.json: %v", err)
			}
			rc.Close()
		case "media/roundtrip.wav":
			foundMedia = true
		}
	}
	if manifest.App != "CuTePi" || manifest.Version != manifestVersion {
		t.Fatalf("unexpected manifest header: %+v", manifest)
	}
	if len(manifest.Cues) != 1 || manifest.Cues[0].Title != "roundtrip.wav" || manifest.Cues[0].Color != "#ff0055" {
		t.Fatalf("manifest cues wrong: %+v", manifest.Cues)
	}
	if manifest.SelectedCuePos != 1 {
		t.Fatalf("manifest selected cue = %d, want 1", manifest.SelectedCuePos)
	}
	if !foundMedia {
		t.Fatal("export did not embed media/roundtrip.wav")
	}

	// Import into a fresh server (fresh in-memory DB and wav dir).
	r2 := setupTestServer(t)

	// A pre-existing cue on the fresh sheet, so "append" has something to
	// follow: the imported cue must land after it, keeping its settings.
	if err := ctp.RegisterMedia("existing.wav", 100, media.Metadata{
		Mimetype: "audio/wav", Duration: 1.0, Codec: "pcm_s16le",
	}, "existing.wav"); err != nil {
		t.Fatalf("RegisterMedia(existing): %v", err)
	}
	if err := ctp.AddCue("existing.wav", ""); err != nil {
		t.Fatalf("AddCue(existing): %v", err)
	}

	postFile := func(mode string) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "show.ctp")
		fw.Write(data)
		mw.WriteField("mode", mode)
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/show/import", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		r2.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther && w.Code != http.StatusOK {
			t.Fatalf("import(%s) = %d, want redirect: %s", mode, w.Code, w.Body.String())
		}
	}

	// Append: the imported cue (title roundtrip.wav) lands after the
	// pre-existing one, keeping its settings.
	postFile("append")
	cues, err := ctp.GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet after append: %v", err)
	}
	if len(cues.Cues) != 2 {
		t.Fatalf("append import: want 2 cues, got %d", len(cues.Cues))
	}
	if cues.Cues[0].Title != "existing.wav" || cues.Cues[1].Title != "roundtrip.wav" {
		t.Fatalf("append import order wrong: %+v", []string{cues.Cues[0].Title, cues.Cues[1].Title})
	}
	if cues.Cues[1].Color != "#ff0055" {
		t.Fatalf("append import lost cue settings: %+v", cues.Cues[1])
	}

	// Overwrite: back to exactly the one exported cue.
	postFile("overwrite")
	cues, err = ctp.GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet after overwrite: %v", err)
	}
	if len(cues.Cues) != 1 || cues.Cues[0].Title != "roundtrip.wav" || cues.Cues[0].Selected != true {
		t.Fatalf("overwrite import wrong: %+v", cues.Cues)
	}

	// Re-export from the fresh server: media must be present again.
	exp2 := get(t, r2, "/api/show/export")
	if exp2.Code != 200 {
		t.Fatalf("re-export = %d: %s", exp2.Code, exp2.Body.String())
	}
	zr2, _ := zip.NewReader(bytes.NewReader(exp2.Body.Bytes()), int64(exp2.Body.Len()))
	anyMedia := false
	for _, f := range zr2.File {
		if strings.HasPrefix(f.Name, "media/") {
			anyMedia = true
		}
	}
	if !anyMedia {
		t.Fatal("re-export after import did not embed the imported media")
	}
}

// buildTinyWav builds a minimal valid WAV (1s of 440 Hz at 8 kHz mono 16-bit)
// so media.Probe and the re-import succeed using real ffprobe.
func buildTinyWav(seconds int) []byte {
	const sampleRate = 8000
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+2*seconds*sampleRate))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(2*seconds*sampleRate))
	for s := 0; s < seconds*sampleRate; s++ {
		v := int16(3000 * math.Sin(2*math.Pi*440*float64(s%sampleRate)/float64(sampleRate)))
		binary.Write(&buf, binary.LittleEndian, v)
	}
	return buf.Bytes()
}

// TestAutoContinueChainFiresAndIsDisarmed is the runnable check for the
// auto-continue wait guard: the chain must fire when nothing intervenes, and
// must be disarmed by ANY operator playback action during the wait —
// including replaying the SAME ending cue, which the old cuePos-equality
// guard could not detect (cuePos stays equal on a replay).
func TestAutoContinueChainFiresAndIsDisarmed(t *testing.T) {
	// Test server setup only (config/DB isolation); the chain logic is
	// exercised directly, not over HTTP.
	setupTestServer(t)

	// DB-only media (no files on disk): buildPipeline succeeds and the swap
	// happens synchronously, while the sink error surfaces later on the bus —
	// same approach as TestGroupPlayNonSlideshowPlaysFirstMember. Keeps the
	// test free of real sink state changes (no audio device in CI).
	for _, name := range []string{"chain-a.wav", "chain-b.wav", "chain-c.wav"} {
		if err := ctp.RegisterMedia(name, 40000, media.Metadata{
			Mimetype: "audio/wav", Duration: 1.0, Codec: "pcm_s16le",
		}, name); err != nil {
			t.Fatalf("RegisterMedia: %v", err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	if err := ctp.UpdateCue("1", "autoContinue", "true"); err != nil {
		t.Fatalf("UpdateCue(autoContinue): %v", err)
	}

	waitForPos := func(want int, d time.Duration) {
		t.Helper()
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if gsp.CurrentCuePos() == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("cue pos stuck at %d, want %d within %v", gsp.CurrentCuePos(), want, d)
	}

	// 1. No intervention: cue 1 ends -> cue 2 fires immediately (postWait 0).
	// Load cue 1 first so the association exists (the old cuePos guard
	// required it; the generation guard does not).
	if err := loadAndPlayCue(mustCue(t, "1")); err != nil {
		t.Fatalf("play cue 1: %v", err)
	}
	autoContinueFrom(1)
	waitForPos(2, 2*time.Second)

	// 2. Replay of the SAME cue during the wait disarms the chain. The old
	// guard (CurrentCuePos() != endingPos) passed here — cuePos is still 1 —
	// so cue 2 wrongly fired after the operator re-triggered cue 1.
	if err := ctp.UpdateCue("1", "postWait", "0.4"); err != nil { // 0.4s
		t.Fatalf("UpdateCue(postWait): %v", err)
	}
	if err := loadAndPlayCue(mustCue(t, "1")); err != nil {
		t.Fatalf("replay cue 1: %v", err)
	}
	autoContinueFrom(1)
	time.Sleep(100 * time.Millisecond)
	if err := loadAndPlayCue(mustCue(t, "1")); err != nil { // operator replays cue 1 mid-wait
		t.Fatalf("operator replay: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if got := gsp.CurrentCuePos(); got != 1 {
		t.Fatalf("replayed cue must disarm the chain: cue pos = %d, want 1 (cue 2 fired)", got)
	}

	// 3. Loading a different cue during the wait disarms the chain too.
	autoContinueFrom(1)
	time.Sleep(100 * time.Millisecond)
	if err := loadAndPlayCue(mustCue(t, "3")); err != nil {
		t.Fatalf("operator loads cue 3: %v", err)
	}
	waitForPos(3, 600*time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	if got := gsp.CurrentCuePos(); got != 3 {
		t.Fatalf("operator load must disarm the chain: cue pos = %d, want 3", got)
	}
}

// mustCue fetches a cue by position or fails the test.
func mustCue(t *testing.T, pos string) ctp.Cue {
	t.Helper()
	cue, err := ctp.GetCue(pos)
	if err != nil {
		t.Fatalf("GetCue(%s): %v", pos, err)
	}
	return cue
}

// TestAuthMiddlewareOptional is the runnable check for the operator
// password: disabled by default (open LAN appliance); when set, every
// request needs HTTP Basic credentials (any username) and wrong/missing
// credentials get a 401 with a WWW-Authenticate challenge. Clearing the
// password restores open access.
func TestAuthMiddlewareOptional(t *testing.T) {
	r := setupTestServer(t)

	// Default: no password, open access.
	if w := get(t, r, "/api/cuesheet"); w.Code != 200 {
		t.Fatalf("GET /api/cuesheet without auth = %d, want 200 (open by default)", w.Code)
	}

	config.SetAuthPassword("showtime")
	defer config.SetAuthPassword("")

	if w := get(t, r, "/api/cuesheet"); w.Code != 401 {
		t.Fatalf("GET /api/cuesheet without credentials = %d, want 401", w.Code)
	}
	if ch := get(t, r, "/api/cuesheet").Header().Get("WWW-Authenticate"); ch == "" {
		t.Fatalf("401 response missing WWW-Authenticate challenge")
	}
	if w := getWithBasic(t, r, "/api/cuesheet", "op", "wrong"); w.Code != 401 {
		t.Fatalf("GET with wrong password = %d, want 401", w.Code)
	}
	if w := getWithBasic(t, r, "/api/cuesheet", "any-user", "showtime"); w.Code != 200 {
		t.Fatalf("GET with correct password = %d, want 200", w.Code)
	}

	// Settings report auth state without leaking the password.
	config.SetAuthPassword("showtime")
	body := getWithBasic(t, r, "/api/settings", "op", "showtime").Body.String()
	if !strings.Contains(body, `"authEnabled":true`) || strings.Contains(body, "showtime") {
		t.Fatalf("settings must expose authEnabled but not the password: %s", body)
	}

	// Clearing restores open access.
	config.SetAuthPassword("")
	if w := get(t, r, "/api/cuesheet"); w.Code != 200 {
		t.Fatalf("GET after clearing password = %d, want 200", w.Code)
	}
}

func getWithBasic(t *testing.T, r *gin.Engine, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	req.SetBasicAuth(user, pass)
	r.ServeHTTP(w, req)
	return w
}

// TestUploadFailurePreservesExistingMedia is the runnable check for the
// temp-write fix: re-uploading a file under a name that already exists in
// the media dir must never destroy the original if the new upload fails
// probe/verify (the old code wrote straight over the file and its failure
// cleanup then deleted the original that live cues point at).
func TestUploadFailurePreservesExistingMedia(t *testing.T) {
	r := setupTestServer(t)
	dir := config.MediaLocation()

	original := []byte("ORIGINAL GOOD MEDIA - cues depend on this file")
	if err := os.WriteFile(filepath.Join(dir, "clip.mp4"), original, 0o644); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	// Garbage payload with a video extension: passes the extension check,
	// fails media.Probe.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("media", "clip.mp4")
	fw.Write([]byte("this is not a real video file"))
	mw.Close()
	req := httptest.NewRequest("POST", "/upload/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("corrupt re-upload = %d, want 422 (got: %s)", w.Code, w.Body.String())
	}

	got, err := os.ReadFile(filepath.Join(dir, "clip.mp4"))
	if err != nil {
		t.Fatalf("original media must survive a failed re-upload: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("failed re-upload destroyed/overwrote the original media file")
	}
	if _, err := os.Stat(filepath.Join(dir, "clip.mp4.uploading")); !os.IsNotExist(err) {
		t.Fatalf("temp sidecar not cleaned up after failure: %v", err)
	}
}

// TestMultiFileUploadKeepsImporting is the runnable check for the
// best-effort multi-file upload: a failure in one file must not silently
// drop the others. The corrupt file comes first to prove the loop keeps
// going past it (the old code aborted the whole batch at the first error).
func TestMultiFileUploadKeepsImporting(t *testing.T) {
	r := setupTestServer(t)
	dir := config.MediaLocation()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("media", "bad.clip.mp4")
	fw.Write([]byte("this is not a real video file"))
	fw, _ = mw.CreateFormFile("media", "good.wav")
	fw.Write(buildTinyWav(1))
	mw.Close()

	req := httptest.NewRequest("POST", "/upload/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("partial-failure upload = %d, want 422: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "bad.clip.mp4") {
		t.Fatalf("error response does not name the failed file: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "good.wav") {
		t.Fatalf("error response blames the file that succeeded: %s", w.Body.String())
	}
	// The good file must have been imported despite the earlier failure.
	if _, err := os.Stat(filepath.Join(dir, "good.wav")); err != nil {
		t.Fatalf("good.wav was not imported after a sibling failed: %v", err)
	}
	pool, err := ctp.GetMediapool()
	if err != nil {
		t.Fatalf("GetMediapool: %v", err)
	}
	found := false
	for _, m := range pool.Medias {
		if m.Filename == "good.wav" {
			found = true
		}
	}
	if !found {
		t.Fatal("good.wav not registered in the mediapool")
	}
}

// buildCTPBytes builds an in-memory .CTP (zip) with the given manifest and
// media entries for import tests.
func buildCTPBytes(t *testing.T, m showManifest, media map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeShowZip(&buf, m); err != nil {
		t.Fatalf("writeShowZip: %v", err)
	}
	// writeShowZip only embeds files present in the media dir; graft extra
	// (possibly bogus) entries on by rewriting with the zip package directly.
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	w, _ := zw.Create("cutepi.json")
	if err := json.NewEncoder(w).Encode(m); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	for name, content := range media {
		w, err := zw.Create("media/" + name)
		if err != nil {
			t.Fatalf("zip entry %q: %v", name, err)
		}
		w.Write(content)
	}
	zw.Close()
	return out.Bytes()
}

// postCTP imports a .CTP payload and returns the recorder.
func postCTP(t *testing.T, r *gin.Engine, data []byte, mode string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "show.ctp")
	fw.Write(data)
	mw.WriteField("mode", mode)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/show/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestShowImportRejectsUnsafeNames is the runnable check for the zip-slip
// guard: traversal names in either the zip entries or the manifest must be
// rejected without writing outside the media dir.
func TestShowImportRejectsUnsafeNames(t *testing.T) {
	r := setupTestServer(t)
	dir := config.MediaLocation()

	manifest := showManifest{
		App: "CuTePi", Version: manifestVersion, SelectedCuePos: 0,
		Cues: []ctp.ExportCue{{Title: "evil", Filename: "../../evil.mp4"}},
	}

	// Traversal in the manifest cue filename.
	w := postCTP(t, r, buildCTPBytes(t, manifest, nil), "append")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("manifest traversal import = %d, want 422 (got: %s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.mp4")); !os.IsNotExist(err) {
		t.Fatalf("import wrote outside the media dir: %v", err)
	}

	// Traversal in a zip entry name (manifest clean, entry evil).
	clean := manifest
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	mw2, _ := zw.Create("cutepi.json")
	json.NewEncoder(mw2).Encode(clean)
	ew, _ := zw.Create("media/../../evil.mp4")
	ew.Write([]byte("x"))
	zw.Close()
	if w := postCTP(t, r, out.Bytes(), "append"); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("zip-slip entry import = %d, want 422 (got: %s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.mp4")); !os.IsNotExist(err) {
		t.Fatalf("zip-slip wrote outside the media dir: %v", err)
	}
}

// TestShowImportFailureKeepsSheet is the runnable check for import ordering:
// a media failure during an OVERWRITE import must not have cleared the
// operator's current cuesheet (the old code cleared the sheet first).
func TestShowImportFailureKeepsSheet(t *testing.T) {
	r := setupTestServer(t)

	// One pre-existing cue the operator would lose on a bad import.
	wav := buildTinyWav(1.0)
	if err := os.WriteFile(filepath.Join(config.MediaLocation(), "mine.wav"), wav, 0o644); err != nil {
		t.Fatalf("seed media: %v", err)
	}
	if err := ctp.RegisterMedia("mine.wav", int64(len(wav)), media.Metadata{Mimetype: "audio/wav", Duration: 1.0, Codec: "pcm_s16le"}, "mine.wav"); err != nil {
		t.Fatalf("RegisterMedia: %v", err)
	}
	if err := ctp.AddCue("mine.wav", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	// Import in overwrite mode: carries "bad.mp4" (garbage payload named like
	// a video) which fails media.Probe during import.
	manifest := showManifest{
		App: "CuTePi", Version: manifestVersion, SelectedCuePos: 0,
		Cues: []ctp.ExportCue{{Title: "bad", Filename: "bad.mp4"}},
	}
	bad := buildCTPBytes(t, manifest, map[string][]byte{"bad.mp4": []byte("not a real video")})
	w := postCTP(t, r, bad, "overwrite")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("import with unprobeable media = %d, want 422 (got: %s)", w.Code, w.Body.String())
	}

	cues, err := ctp.GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(cues.Cues) != 1 || cues.Cues[0].Title != "mine.wav" {
		t.Fatalf("failed import destroyed the operator's sheet: %+v", cues.Cues)
	}
}

// TestShowImportAppendKeepsExistingCues is the runnable check for the append
// rollback range in the import handler: AddCueFull always appends (it ignores
// the exported cuePos), so append-mode imports land at appendedOffset+1.. and
// never displace the operator's existing sheet - the invariant the failure
// rollback's position range depends on.
func TestShowImportAppendKeepsExistingCues(t *testing.T) {
	r := setupTestServer(t)

	wav := buildTinyWav(1.0)
	reg := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(config.MediaLocation(), name), wav, 0o644); err != nil {
			t.Fatalf("seed media %s: %v", name, err)
		}
		if err := ctp.RegisterMedia(name, int64(len(wav)), media.Metadata{Mimetype: "audio/wav", Duration: 1.0, Codec: "pcm_s16le"}, name); err != nil {
			t.Fatalf("RegisterMedia(%s): %v", name, err)
		}
	}
	reg("mine.wav")
	reg("a.wav")
	reg("b.wav")
	if err := ctp.AddCue("mine.wav", ""); err != nil {
		t.Fatalf("AddCue: %v", err)
	}

	manifest := showManifest{
		App: "CuTePi", Version: manifestVersion, SelectedCuePos: 0,
		Cues: []ctp.ExportCue{
			{Title: "a", Filename: "a.wav", CueNum: "1"},
			{Title: "b", Filename: "b.wav", CueNum: "2"},
		},
	}
	if w := postCTP(t, r, buildCTPBytes(t, manifest, nil), "append"); w.Code != http.StatusSeeOther {
		t.Fatalf("append import = %d, want 303 (got: %s)", w.Code, w.Body.String())
	}

	cues, err := ctp.GetCuesheet()
	if err != nil {
		t.Fatalf("GetCuesheet: %v", err)
	}
	if len(cues.Cues) != 3 {
		t.Fatalf("imported cue count = %d, want 3: %+v", len(cues.Cues), cues.Cues)
	}
	if cues.Cues[0].Title != "mine.wav" || cues.Cues[1].Title != "a" || cues.Cues[2].Title != "b" {
		t.Fatalf("append import displaced existing cues: %+v", cues.Cues)
	}
}

// TestUploadBodyLimit is the runnable check for the body cap: a request
// larger than maxBodyBytes is rejected instead of being spooled to disk.
func TestUploadBodyLimit(t *testing.T) {
	r := setupTestServer(t)
	old := maxBodyBytes
	maxBodyBytes = 1 << 20 // 1 MiB for the test
	defer func() { maxBodyBytes = old }()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("media", "big.mp4")
	if _, err := io.Copy(fw, strings.NewReader(strings.Repeat("x", 2<<20))); err != nil { // 2 MiB payload
		t.Fatalf("building oversized upload: %v", err)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/upload/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(buf.Len())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code < 400 {
		t.Fatalf("oversized upload = %d, want an error status; body: %.200s", w.Code, w.Body.String())
	}
	// Nothing may have landed in the media dir.
	if entries, _ := os.ReadDir(config.MediaLocation()); len(entries) != 0 {
		t.Fatalf("oversized upload left files behind: %v", entries)
	}
}

// TestYoutubeDlpTimeout is the runnable check for the yt-dlp timeout: a hung
// yt-dlp subprocess is killed after ytDlpResolveTimeout and the request
// fails instead of pinning the HTTP handler forever; the temp download dir
// is cleaned up.
func TestYoutubeDlpTimeout(t *testing.T) {
	setupTestServer(t) // config dirs for the temp download location
	old := ytDlpResolveTimeout
	ytDlpResolveTimeout = 300 * time.Millisecond
	defer func() { ytDlpResolveTimeout = old }()

	// Fake yt-dlp that hangs forever (exec so the kill reaches the sleeper,
	// not just the shell wrapping it).
	bin := t.TempDir()
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake yt-dlp: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	_, _, err := downloadWithYtDlp("https://youtube.com/watch?v=x", nil)
	if err == nil {
		t.Fatal("hung yt-dlp must fail via timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, subprocess not killed promptly", elapsed)
	}
	entries, _ := os.ReadDir(config.MediaLocation())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ytdlp-") {
			t.Fatalf("temp download dir left behind: %s", e.Name())
		}
	}
}

// TestGoSafeRecoversPanic is the runnable check for goSafe: a panic in a
// background goroutine (fade chain, auto-continue timer) is converted into
// a log line instead of killing the process - Gin's Recovery middleware
// never sees these goroutines.
func TestGoSafeRecoversPanic(t *testing.T) {
	done := make(chan struct{})
	goSafe(func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("panicking goroutine killed before its defer ran")
	}
	// The process is still alive and the hub works: prove it by hitting the
	// test server once more.
	if w := get(t, setupTestServer(t), "/api/cuesheet"); w.Code != 200 {
		t.Fatalf("server dead after recovered panic: %d", w.Code)
	}
}

// The Advance checkbox moved from the header GO cluster into the settings
// modal: toggling posts /api/setting/goadvance and the choice round-trips
// through GET /api/settings (which populates the modal's checkbox).
func TestGoAdvanceRoundTrip(t *testing.T) {
	r := setupTestServer(t)

	off := httptest.NewRequest("POST", "/api/setting/goadvance", strings.NewReader(""))
	off.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, off)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("POST /api/setting/goadvance (off) = %d, want 2xx", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(get(t, r, "/api/settings").Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /api/settings: %v", err)
	}
	if body["goAdvance"] != false {
		t.Fatalf("goAdvance = %v, want false: %s", body["goAdvance"], body)
	}

	on := httptest.NewRequest("POST", "/api/setting/goadvance", strings.NewReader("advance=1"))
	on.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, on)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("POST /api/setting/goadvance (on) = %d, want 2xx", w.Code)
	}
	if err := json.Unmarshal(get(t, r, "/api/settings").Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /api/settings: %v", err)
	}
	if body["goAdvance"] != true {
		t.Fatalf("goAdvance = %v, want true: %s", body["goAdvance"], body)
	}

	if html := get(t, r, "/").Body.String(); strings.Contains(html, `id="go-advance"`) {
		t.Fatal("header still renders the Advance checkbox")
	}
}

// Edit/Show mode (footer toggle): posts flip the persisted mode, which
// round-trips through GET /api/settings and seeds the index body class.
func TestShowModeRoundTrip(t *testing.T) {
	r := setupTestServer(t)

	post := func(v string) int {
		req := httptest.NewRequest("POST", "/api/setting/showmode", strings.NewReader(v))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := post("showmode=1"); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("POST /api/setting/showmode (on) = %d, want 2xx", code)
	}
	var body map[string]any
	if err := json.Unmarshal(get(t, r, "/api/settings").Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /api/settings: %v", err)
	}
	if body["showMode"] != true {
		t.Fatalf("showMode = %v, want true", body["showMode"])
	}
	if html := get(t, r, "/").Body.String(); !strings.Contains(html, `class="app-body show-mode"`) {
		t.Fatal("index body missing show-mode class after enabling")
	}
	if code := post(""); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("POST /api/setting/showmode (off) = %d, want 2xx", code)
	}
	if err := json.Unmarshal(get(t, r, "/api/settings").Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /api/settings: %v", err)
	}
	if body["showMode"] != false {
		t.Fatalf("showMode = %v, want false", body["showMode"])
	}
}

// The footer carries the panel toggles, Tests, Settings and mode switch.
func TestIndexRendersFooter(t *testing.T) {
	r := setupTestServer(t)
	body := get(t, r, "/").Body.String()
	for _, want := range []string{
		`id="app-footer"`,
		`id="mode-toggle"`,
		`data-settings-open`,
		`&middot;`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected the index page to contain %q", want)
		}
	}
}

// Multi-selection (§12.4): toggle adds a highlighted member row, extend
// selects the anchor-to-click range. Members share the anchor's
// table-warning highlight — one selection look for every selected row.
func TestMultiSelectToggleAndExtend(t *testing.T) {
	r := setupTestServer(t)
	for _, f := range []string{"ms-1.mp4", "ms-2.mp4", "ms-3.mp4"} {
		if err := ctp.RegisterMedia(f, 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, f); err != nil {
			t.Fatal(err)
		}
		if err := ctp.AddCue(f, ""); err != nil {
			t.Fatal(err)
		}
	}
	if w := post(t, r, "/api/cue/1"); w.Code != 200 {
		t.Fatalf("select anchor = %d", w.Code)
	}
	if w := post(t, r, "/api/cue/3?toggle=1"); w.Code != 200 {
		t.Fatalf("toggle = %d", w.Code)
	} else if !strings.Contains(w.Body.String(), `data-cue-sel="1"`) {
		t.Fatal("toggled member missing the selected mark")
	}
	if w := post(t, r, "/api/cue/2?extend=1"); w.Code != 200 {
		t.Fatalf("extend = %d", w.Code)
	} else {
		body := w.Body.String()
		if strings.Count(body, `data-cue-sel="1"`) < 2 {
			t.Fatalf("range 1..3 should mark 2 members selected, got:\n%s", body)
		}
	}
}

// The schedule-next endpoint arms the GO flash: disarmed in Edit mode,
// due-in seconds when a schedule approaches in Show mode.
func TestScheduleNextEndpoint(t *testing.T) {
	r := setupTestServer(t)
	if err := ctp.RegisterMedia("sn.mp4", 100, media.Metadata{Mimetype: "video/mp4", Duration: 10}, "sn.mp4"); err != nil {
		t.Fatal(err)
	}
	if err := ctp.AddCue("sn.mp4", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	day := int(now.Weekday())
	if day == 0 {
		day = 7
	}
	// Due 30 minutes from now (inside the day, safely in the future).
	futureSec := now.Hour()*3600 + now.Minute()*60 + now.Second() + 1800
	if futureSec >= 24*3600 {
		t.Skip("too close to midnight for a same-day schedule")
	}
	if err := ctp.SetCueSchedule(1, true, day, futureSec); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, "/api/schedule/next").Body.String(); !strings.Contains(got, `"armed":false`) {
		t.Fatalf("edit mode should disarm the flash, got: %s", got)
	}
	postForm(t, r, "/api/setting/showmode", "showmode", "1")
	got := get(t, r, "/api/schedule/next").Body.String()
	if !strings.Contains(got, `"armed":true`) || !strings.Contains(got, `"dueInSec"`) {
		t.Fatalf("show mode should report the upcoming cue, got: %s", got)
	}
	postForm(t, r, "/api/setting/showmode", "showmode", "")
}
