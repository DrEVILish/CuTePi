package routes

import "testing"

// The 1080p60 table follows the codec support tests: HEVC only on the GPU
// wall, H.264 and the fast software codecs on both, VP9/AV1 converted.
func TestPlays60(t *testing.T) {
	for _, c := range []struct {
		codec    string
		gpu, kms bool
	}{
		{"h264", true, true}, {"hevc", true, false}, {"vp9", false, false}, {"av1", false, false},
		{"vp8", true, true}, {"mpeg2video", true, true}, {"theora", false, true}, {"prores", false, false},
		{"cfhd", false, false}, {"", false, false},
	} {
		if got := plays60(c.codec, true); got != c.gpu {
			t.Errorf("%q on the GPU wall: %v, want %v", c.codec, got, c.gpu)
		}
		if got := plays60(c.codec, false); got != c.kms {
			t.Errorf("%q on the KMS wall: %v, want %v", c.codec, got, c.kms)
		}
	}
	if _, to := convertArgs(true, false); to != "hevc" {
		t.Errorf("GPU wall target %q, want hevc", to)
	}
	if _, to := convertArgs(false, true); to != "h264" {
		t.Errorf("KMS wall target %q, want h264", to)
	}
}

// Of YouTube's 1080p60 streams (AV1, VP9, H.264) the H.264 one is taken on
// both walls; a lower-resolution H.264 never wins over a better picture.
func TestChooseFormat(t *testing.T) {
	yt := []ytFormat{
		{"399", "av01.0.09M.08", 1080, 60, 1568}, {"303", "vp9", 1080, 60, 2127},
		{"299", "avc1.64002A", 1080, 60, 3248}, {"298", "avc1.4d4020", 720, 60, 1500},
		{"401", "av01.0.12M.08", 2160, 60, 9000}, {"140", "none", 0, 0, 128},
	}
	if got := chooseFormat(yt, 1080, true); got != "299" {
		t.Errorf("GPU wall: %q, want 299", got)
	}
	if got := chooseFormat(yt, 1080, false); got != "299" {
		t.Errorf("KMS wall: %q, want 299", got)
	}
	noH264At1080 := []ytFormat{{"399", "av01.0.09M.08", 1080, 60, 1568}, {"298", "avc1.4d4020", 720, 60, 1500}}
	if got := chooseFormat(noH264At1080, 1080, true); got != "" {
		t.Errorf("only AV1 at 1080: %q, want \"\" (best picture, converted)", got)
	}
}
