package gsp

import (
	"os"
	"testing"
)

// The decoder rank overrides are appended to the operator's own ranks, and an
// element the operator already ranks keeps the operator's value.
func TestApplyDecoderRanks(t *testing.T) {
	old, had := os.LookupEnv("GST_PLUGIN_FEATURE_RANK")
	defer func() {
		if had {
			os.Setenv("GST_PLUGIN_FEATURE_RANK", old)
		} else {
			os.Unsetenv("GST_PLUGIN_FEATURE_RANK")
		}
	}()
	cases := []struct{ in, want string }{
		{"", "v4l2jpegdec:0,openjpegdec:0,avdec_vp8:257"},
		{"avdec_h264:300", "avdec_h264:300,v4l2jpegdec:0,openjpegdec:0,avdec_vp8:257"},
		{"v4l2jpegdec:256", "v4l2jpegdec:256,openjpegdec:0,avdec_vp8:257"},
		{"openjpegdec:300,v4l2jpegdec:256,avdec_vp8:64", "openjpegdec:300,v4l2jpegdec:256,avdec_vp8:64"},
	}
	for _, c := range cases {
		os.Setenv("GST_PLUGIN_FEATURE_RANK", c.in)
		applyDecoderRanks()
		if got := os.Getenv("GST_PLUGIN_FEATURE_RANK"); got != c.want {
			t.Errorf("from %q: got %q, want %q", c.in, got, c.want)
		}
	}
}
