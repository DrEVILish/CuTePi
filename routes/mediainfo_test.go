package routes

import (
	"strings"
	"testing"

	"CuTePi/ctp"
)

// Stills get a curated Media tab: operator-worded type, real resolution,
// pixel format and file size — never container internals ("png_pipe"), the
// bogus frame rate ffprobe implies for stills, or zero bitrates.
func TestMediaRowsImageCurated(t *testing.T) {
	cue := ctp.Cue{
		Media: ctp.Media{
			Filename:   "still.png",
			Mimetype:   "image/png",
			Size:       31179,
			Resolution: "1920x1080",
		},
	}
	cue.MediaInfo = `{"Container":"png_pipe","OverallBitrate":0,"Duration":0,` +
		`"Video":{"Codec":"png","Profile":"","Width":1920,"Height":1080,` +
		`"FPS":25,"PixFmt":"rgb24","Bitrate":0,"Color":"gbr"},"Audio":null}`
	rows := mediaInfoRows(cue)
	joined := ""
	for _, r := range rows {
		joined += r.Label + "=" + r.Value + ";"
	}
	for _, want := range []string{"Type=PNG image", "Resolution=1920 x 1080", "Pixel format=rgb24"} {
		if !strings.Contains(joined, want) {
			t.Errorf("image rows %q missing %q", joined, want)
		}
	}
	for _, banned := range []string{"Frame rate", "png_pipe", "bitrate"} {
		if strings.Contains(strings.ToLower(joined), strings.ToLower(banned)) {
			t.Errorf("image rows %q must not contain %q", joined, banned)
		}
	}
}

// Unknown still codecs degrade to "<CODEC> image", never to nothing.
func TestFriendlyImageKind(t *testing.T) {
	cases := map[string]string{
		"png": "PNG image", "mjpeg": "JPEG image", "gif": "GIF image",
		"tiff": "TIFF image", "": "Image",
	}
	for in, want := range cases {
		if got := friendlyImageKind(in); got != want {
			t.Errorf("friendlyImageKind(%q) = %q, want %q", in, got, want)
		}
	}
}
