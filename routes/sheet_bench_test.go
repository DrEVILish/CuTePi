package routes

import (
	"fmt"
	"html/template"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// The UI's request paths on a show-sized sheet: 80 video cues over 10 clips,
// each clip with a full-size waveform (~360 KB of peaks, as a long clip
// gets), a video cue selected. The database is on disk (not :memory:), under
// /var/tmp when it exists, so writes pay a real commit.
//
//	go test ./routes/ -run XXX -bench Sheet -benchtime 2s
func benchSheet(b *testing.B) *gin.Engine {
	b.Helper()
	dir := b.TempDir()
	dbDir := dir
	if d, err := os.MkdirTemp("/var/tmp", "cutepi-bench-"); err == nil {
		dbDir = d
		b.Cleanup(func() { os.RemoveAll(d) })
	}
	config.SetDbLocation(dbDir + "/ctp.db")
	config.SetConfigFilePath(dir + "/config.json")
	config.SetDirsForTesting(dir)
	if err := ctp.InitDB(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { ctp.CloseDB() })
	var wave strings.Builder
	wave.WriteByte('[')
	for i := 0; i < 60000; i++ {
		if i > 0 {
			wave.WriteByte(',')
		}
		fmt.Fprintf(&wave, "0.%03d", (i*37)%1000)
	}
	wave.WriteByte(']')
	for m := 0; m < 10; m++ {
		name := fmt.Sprintf("clip%02d.mp4", m)
		info := &media.MediaInfo{Container: "mov,mp4", Duration: 600, Video: &media.MediaVideoInfo{Codec: "h264", Width: 1920, Height: 1080}, Audio: &media.MediaAudioInfo{Codec: "aac"}}
		if err := ctp.RegisterMedia(name, 1<<30, media.Metadata{Mimetype: "video/mp4", Duration: 600, Resolution: "1920x1080", Codec: "h264", Info: info}, name); err != nil {
			b.Fatal(err)
		}
		pool, _ := ctp.GetMediapool()
		for _, p := range pool.Medias {
			if p.Filename == name {
				if err := ctp.StoreWaveform(p.Media_id, wave.String()); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	for c := 0; c < 80; c++ {
		if err := ctp.AddCue(fmt.Sprintf("clip%02d.mp4", c%10), ""); err != nil {
			b.Fatal(err)
		}
	}
	if err := ctp.SetCue("1"); err != nil {
		b.Fatal(err)
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.SetHTMLTemplate(template.Must(template.New("").Funcs(TemplateFuncs()).ParseGlob("../templates/*")))
	Index(r.Group("/"))
	Api(r.Group("/api"))
	Groups(r.Group("/api"))
	return r
}

func benchRequest(b *testing.B, method string, path func(i int) string, form url.Values) {
	r := benchSheet(b)
	b.ResetTimer()
	var bytes int
	for i := 0; i < b.N; i++ {
		var body *strings.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path(i), body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			b.Fatalf("%s %s: %d %s", method, path(i), w.Code, w.Body.String())
		}
		bytes = w.Body.Len()
	}
	b.ReportMetric(float64(bytes), "B/resp")
}

func fixed(p string) func(int) string { return func(int) string { return p } }

func BenchmarkSheetIndex(b *testing.B)      { benchRequest(b, "GET", fixed("/"), nil) }
func BenchmarkSheetCuesheet(b *testing.B)   { benchRequest(b, "GET", fixed("/api/cuesheet"), nil) }
func BenchmarkSheetNowplaying(b *testing.B) { benchRequest(b, "GET", fixed("/api/nowplaying"), nil) }
func BenchmarkSheetInspector(b *testing.B)  { benchRequest(b, "GET", fixed("/api/cue/inspector"), nil) }
func BenchmarkSheetMediapool(b *testing.B)  { benchRequest(b, "GET", fixed("/mediapool"), nil) }

// A row click: selection is written, the sheet comes back.
func BenchmarkSheetSelect(b *testing.B) {
	benchRequest(b, "POST", func(i int) string { return fmt.Sprintf("/api/cue/%d", 1+i%80) }, nil)
}

// An inspector edit (volume) on the selected cue.
func BenchmarkSheetInspectorSave(b *testing.B) {
	benchRequest(b, "PUT", fixed("/api/cue/inspector/1"), url.Values{"volume": {"-3"}, "stop_others_shown": {"1"}, "stop_others": {"on"}})
}
