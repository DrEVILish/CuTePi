package gsp

import "testing"

// Without a KMS wall pages render in process, where standing by gains
// nothing: StandbyLive must not build anything, and a GO finds no standby.
func TestLiveStandbyOffWithoutKMSWall(t *testing.T) {
	if kmsWall() != nil {
		t.Skip("KMS wall open")
	}
	if err := StandbyLive([]LiveStandbyWant{{Title: "Page", URL: "http://example.invalid/", Opts: LoadOpts{FadeIn: 500}}}, true); err != nil {
		t.Fatal(err)
	}
	if n := StandbyLiveCount(); n != 0 {
		t.Fatalf("%d pages standing by without a KMS wall", n)
	}
	if p := takeStandbyLive("Page", "http://example.invalid/", liveOpts(LoadOpts{FadeIn: 500})); p != nil {
		t.Fatal("GO took a standby page that was never built")
	}
	// The GO's options and the standby's compare equal after liveOpts, however
	// often it is applied.
	o := LoadOpts{FadeIn: 500, Hold: true, Loop: true, Rate: 2, InPoint: 3, WarmPreroll: true}
	if liveOpts(o) != liveOpts(liveOpts(o)) {
		t.Fatal("liveOpts is not idempotent")
	}
}

// The pool is sized by memory: 3 pages on a 4 GB Pi, 1 on 2 GB and below
// (also when /proc/meminfo is unreadable).
func TestLiveStandbyCap(t *testing.T) {
	for _, c := range []struct{ kb, want int }{{3886080, 3}, {8 << 20, 3}, {1919488, 1}, {1 << 20, 1}, {0, 1}, {2560 << 10, 2}} {
		if got := liveStandbyCap(c.kb); got != c.want {
			t.Errorf("liveStandbyCap(%d kB) = %d, want %d", c.kb, got, c.want)
		}
	}
}
