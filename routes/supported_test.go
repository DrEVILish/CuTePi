package routes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The supported set follows the codec support tests: HEVC only on the GPU
// wall, Theora only on the KMS wall, 8-bit H.264 only; VP9, AV1 and the
// rest are refused.
func TestPlays60(t *testing.T) {
	for _, c := range []struct {
		codec, pix   string
		gpu, pw, kms bool
	}{
		{"h264", "yuv420p", true, true, true}, {"h264", "yuv420p10le", false, false, false}, {"h264", "", true, true, true},
		{"hevc", "yuv420p", true, true, false}, {"hevc", "yuv420p10le", true, true, false},
		{"vp9", "yuv420p", false, true, false}, {"av1", "yuv420p", false, false, false},
		{"vp8", "", true, true, true}, {"mpeg2video", "", true, true, true}, {"theora", "", false, true, true},
		{"prores", "yuv422p10le", false, false, false}, {"cfhd", "", false, false, false}, {"", "", false, false, false},
	} {
		for _, w := range []struct {
			p    displayPath
			want bool
		}{{pathGPU, c.gpu}, {pathPresenter, c.pw}, {pathKMS, c.kms}} {
			if got := plays60(c.codec, c.pix, w.p); got != w.want {
				t.Errorf("%s/%s on path %d: %v, want %v", c.codec, c.pix, w.p, got, w.want)
			}
		}
	}
}

// The notice names every supported codec for the wall in use, and the
// containers.
func TestSupportedNotice(t *testing.T) {
	gpu, kms := supportedNotice(pathGPU), supportedNotice(pathKMS)
	for _, want := range []string{"H.264", "HEVC", "VP8", "MPEG-2", "Motion JPEG", "MP4, MOV, MKV or WebM", "not converted"} {
		if !strings.Contains(gpu, want) {
			t.Errorf("GPU wall notice lacks %q: %s", want, gpu)
		}
	}
	if strings.Contains(gpu, "Theora") || strings.Contains(kms, "HEVC") || !strings.Contains(kms, "Theora") {
		t.Errorf("notices do not follow the wall: GPU %q, KMS %q", gpu, kms)
	}
	if pw := supportedNotice(pathPresenter); !strings.Contains(pw, "VP9") || !strings.Contains(pw, "HEVC") {
		t.Errorf("presenter notice: %q", pw)
	}
}

// Of YouTube's 1080p60 streams (AV1, VP9, H.264) the H.264 one is taken on
// both walls; a lower-resolution H.264 never wins over a better picture, so
// a video offered at its best only in AV1 is refused, with AV1 named.
func TestChooseFormat(t *testing.T) {
	yt := []ytFormat{
		{"399", "av01.0.09M.08", 1080, 60, 1568}, {"303", "vp9", 1080, 60, 2127},
		{"299", "avc1.64002A", 1080, 60, 3248}, {"298", "avc1.4d4020", 720, 60, 1500},
		{"401", "av01.0.12M.08", 2160, 60, 9000}, {"140", "none", 0, 0, 128},
	}
	if got := chooseFormat(yt, 1080, pathGPU); got != "299" {
		t.Errorf("GPU wall: %q, want 299", got)
	}
	if got := chooseFormat(yt, 1080, pathKMS); got != "299" {
		t.Errorf("KMS wall: %q, want 299", got)
	}
	noH264At1080 := []ytFormat{{"399", "av01.0.09M.08", 1080, 60, 1568}, {"298", "avc1.4d4020", 720, 60, 1500}}
	id, offered := chooseFormatOffered(noH264At1080, 1080, pathGPU)
	if id != "" || len(offered) != 1 || offered[0] != "av1" {
		t.Errorf("only AV1 at 1080: %q offered %v, want \"\" and [av1] (refused)", id, offered)
	}
}

// A URL whose video the site offers at its best only in AV1 and VP9 is
// refused before anything is downloaded: the stream ends with an error that
// names the codecs offered and lists what is supported; no download runs.
func TestYoutubeRefusesUnsupported(t *testing.T) {
	r := setupTestServer(t)
	bin := t.TempDir()
	marker := filepath.Join(bin, "downloaded")
	script := "#!/bin/sh\ncase \"$*\" in *-J*) echo '{\"formats\":[" +
		"{\"format_id\":\"399\",\"vcodec\":\"av01.0.09M.08\",\"height\":1080,\"fps\":60,\"tbr\":1500}," +
		"{\"format_id\":\"303\",\"vcodec\":\"vp9\",\"height\":1080,\"fps\":60,\"tbr\":2100}," +
		"{\"format_id\":\"298\",\"vcodec\":\"avc1.4d4020\",\"height\":720,\"fps\":60,\"tbr\":1500}]}' ;;\n" +
		"*) touch " + marker + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "yt-dlp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	w := postForm(t, r, "/youtube", "url", "https://youtube.com/watch?v=x")
	body := w.Body.String()
	for _, want := range []string{`"unsupported":true`, "Not imported", "AV1, VP9", "H.264 (8-bit)", "MP4, MOV, MKV or WebM"} {
		if !strings.Contains(body, want) {
			t.Errorf("response lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, `"done"`) {
		t.Errorf("an unsupported video was imported: %s", body)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("yt-dlp downloaded a video that was going to be refused")
	}
}

// An upload GStreamer cannot play is refused with the supported list; other
// refusals (a bad name) are not padded with it.
func TestUploadFailureMessage(t *testing.T) {
	m := uploadFailureMessage([]string{`"a.mkv": cannot be played: no decoder`}, 2, pathGPU)
	if !strings.HasPrefix(m, "could not import 1 of 2") || !strings.Contains(m, "Supported video") {
		t.Errorf("unplayable upload: %q", m)
	}
	if m := uploadFailureMessage([]string{`"..": invalid filename`}, 1, pathGPU); strings.Contains(m, "Supported video") {
		t.Errorf("bad name refusal lists codecs: %q", m)
	}
}
