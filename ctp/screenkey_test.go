package ctp

import (
	"strconv"
	"strings"
	"testing"
)

// A screen key pasted in a URL is split off: the cue keeps the clean URL,
// the renderer's URL carries the key, and the key goes with the last page
// that uses it.
func TestScreenKeyKeptApart(t *testing.T) {
	if err := ClearCueSheet(); err != nil {
		t.Fatal(err)
	}
	pos, err := AddLiveCue("Room A", "http://timer.example/d/K7QPM3XB?screen=Stage&key=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	cue, err := GetCue(strconv.Itoa(pos))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cue.EndpointURL, "s3cret") || strings.Contains(cue.EndpointURL, "key=") {
		t.Fatalf("cue URL keeps the key: %q", cue.EndpointURL)
	}
	if !HasScreenKey(cue.EndpointURL) {
		t.Fatal("key not stored")
	}
	if got := WithScreenKey(cue.EndpointURL); !strings.Contains(got, "key=s3cret") || !strings.Contains(got, "screen=Stage") {
		t.Fatalf("renderer URL = %q", got)
	}
	exported, _, err := ExportCues()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range exported {
		if strings.Contains(c.EndpointURL, "s3cret") {
			t.Fatalf("export carries the key: %q", c.EndpointURL)
		}
	}
	if got := RedactScreenKey(`load "http://h/d/X?key=s3cret&screen=A" failed`); strings.Contains(got, "s3cret") {
		t.Fatalf("redaction kept the key: %q", got)
	}
	if err := RemoveCue(strconv.Itoa(pos)); err != nil {
		t.Fatal(err)
	}
	if err := PruneEndpoints(); err != nil {
		t.Fatal(err)
	}
	if HasScreenKey(cue.EndpointURL) {
		t.Fatal("key outlived its last page")
	}
}

func TestWithScreenName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://timer.example/d/", "https://timer.example/d/?screen=cutepi"},
		{"https://timer.example/d/ABC123?view=stage", "https://timer.example/d/ABC123?screen=cutepi&view=stage"},
		{"https://timer.example/d/ABC123?screen=Stage+Left", "https://timer.example/d/ABC123?screen=Stage+Left"},
		{"https://www.bbc.co.uk/news", "https://www.bbc.co.uk/news"},
		{"https://timer.example/dashboard", "https://timer.example/dashboard"},
	} {
		if got := WithScreenName(c.in, "cutepi"); got != c.want {
			t.Errorf("WithScreenName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeAlign(t *testing.T) {
	for in, want := range map[string]string{"": "", " right  top ": "right top", "bottom": "bottom", "Center Middle": "center middle"} {
		if got, err := normalizeAlign(in); err != nil || got != want {
			t.Errorf("normalizeAlign(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"left right", "top bottom", "up"} {
		if _, err := normalizeAlign(bad); err == nil {
			t.Errorf("normalizeAlign(%q) accepted", bad)
		}
	}
}
