package routes

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/media"
)

// awardsGstAvailable skips tests that need a real pipeline when GStreamer
// is absent.
func awardsGstAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		t.Skip("gst-launch-1.0 not available; skipping awards playback test")
	}
}

// awardsOrder without shuffle is the sheet order, untouched.
func TestAwardsOrderSequence(t *testing.T) {
	m := []int{4, 1, 9}
	got := awardsOrder(m, false, rand.New(rand.NewSource(1)))
	if len(got) != 3 || got[0] != 4 || got[1] != 1 || got[2] != 9 {
		t.Fatalf("sequence order changed: %v", got)
	}
	if len(m) != 3 || m[0] != 4 {
		t.Fatalf("awardsOrder must not mutate its input: %v", m)
	}
}

// awardsOrder with shuffle is a permutation (same members, no loss, no dup).
func TestAwardsOrderIsPermutation(t *testing.T) {
	m := []int{1, 2, 3, 4, 5}
	seen := map[int]int{}
	for seed := int64(0); seed < 20; seed++ {
		got := awardsOrder(m, true, rand.New(rand.NewSource(seed)))
		if len(got) != len(m) {
			t.Fatalf("seed %d: length %d, want %d", seed, len(got), len(m))
		}
		seen[got[0]]++
		once := map[int]bool{}
		for _, v := range got {
			if once[v] {
				t.Fatalf("seed %d: duplicate %d in %v", seed, v, got)
			}
			once[v] = true
		}
	}
	// Shuffling actually shuffles (20 seeds never all agree on first pick
	// would be a 5^-20 miracle; the property that matters is above).
	if len(seen) < 2 {
		t.Fatalf("shuffle never varied: %v", seen)
	}
}

// PUT /api/group/:id enforces the exclusive modes: awards wins when both
// are sent, and each clears the other on its own.
func TestAwardsPUTExclusivity(t *testing.T) {
	r := setupTestServer(t)
	gid, err := ctp.CreateGroup("Awards", 0)
	if err != nil {
		t.Fatal(err)
	}
	put := func(kv ...string) {
		t.Helper()
		w := putForm(t, r, "/api/group/"+strconv.Itoa(gid), kv...)
		if w.Code != 200 {
			t.Fatalf("PUT = %d: %s", w.Code, w.Body.String())
		}
	}
	put("awards", "true", "slideshow", "true")
	if g, _ := ctp.GetGroup(gid); !g.AwardsMode || g.Slideshow {
		t.Fatalf("both sent: want awards-only, got %+v", g)
	}
	put("slideshow", "true")
	if g, _ := ctp.GetGroup(gid); !g.Slideshow || g.AwardsMode {
		t.Fatalf("slideshow alone: want slideshow-only, got %+v", g)
	}
	// The inspector renders the awards checkbox checked.
	w := get(t, r, "/api/group/"+strconv.Itoa(gid)+"/inspector")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="awards"`) {
		t.Fatalf("inspector missing awards control: %d", w.Code)
	}
	put("awards", "true")
	w = get(t, r, "/api/group/"+strconv.Itoa(gid)+"/inspector")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `name="awards"`) ||
		!strings.Contains(body, "Awards mode: GO plays one member") {
		t.Fatalf("inspector missing awards banner: %d", w.Code)
	}
}

// awardsFixture builds an awards group with two real wav members plus a
// following top-level cue (the continue-out target).
func awardsFixture(t *testing.T) (gid, m1, m2, m3 int) {
	t.Helper()
	setupTestDB(t)
	// Long members: a natural end-of-stream mid-test would take the stale
	// restart path instead of the stop path under test.
	addWav := func(name string) int {
		t.Helper()
		if err := os.WriteFile(filepath.Join(config.MediaLocation(), name), buildTinyWav(30), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ctp.RegisterMedia(name, 100, media.Metadata{Mimetype: "audio/wav", Duration: 30}, name); err != nil {
			t.Fatal(err)
		}
		if err := ctp.AddCue(name, ""); err != nil {
			t.Fatal(err)
		}
		sheet, err := ctp.GetCuesheet()
		if err != nil {
			t.Fatal(err)
		}
		max := 0
		for _, c := range sheet.Cues {
			if c.CuePos > max {
				max = c.CuePos
			}
		}
		return max
	}
	m1, m2 = addWav("aw1.wav"), addWav("aw2.wav")
	gid, err := ctp.CreateGroup("Awards", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ctp.SetCueGroup(strconv.Itoa(m1), gid); err != nil {
		t.Fatal(err)
	}
	if err := ctp.SetCueGroup(strconv.Itoa(m2), gid); err != nil {
		t.Fatal(err)
	}
	// The continue-out target: a top-level cue AFTER the group block.
	m3 = addWav("aw3.wav")
	g, err := ctp.GetGroup(gid)
	if err != nil {
		t.Fatal(err)
	}
	g.AwardsMode = true
	g.FadeMS = 0 // deterministic: stop lands synchronously in the test poll
	if err := ctp.UpdateGroup(g); err != nil {
		t.Fatal(err)
	}
	gsp.Panic()
	awardsSelectionLeft(0)
	if err := ctp.SetSelectedGroup(gid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gsp.Panic()
		awardsSelectionLeft(0)
	})
	return gid, m1, m2, m3
}

func awardsWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The full GO cycle on a 2-member awards group: play m1 (selection stays),
// stop m1 (selection stays), play m2, stop m2 (last, no loop) → selection
// steps out and the next cue fires like a normal GO.
func TestAwardsFireCycle(t *testing.T) {
	awardsGstAvailable(t)
	gid, m1, m2, m3 := awardsFixture(t)

	if err := FireSelected(); err != nil {
		t.Fatalf("GO 1: %v", err)
	}
	awardsWaitFor(t, "m1 playing", func() bool { return gsp.CurrentPlaying() == "aw1.wav" })
	if gid2, _ := ctp.SelectedGroupPos(); gid2 != gid {
		t.Fatalf("GO 1 moved selection off the group: group=%d", gid2)
	}
	if !awardsSuppressAutoContinue(m1) {
		t.Fatalf("playing member should suppress auto-continue")
	}

	if err := FireSelected(); err != nil {
		t.Fatalf("GO 2: %v", err)
	}
	awardsWaitFor(t, "m1 stopped", func() bool { return gsp.CurrentPlaying() == "" })
	if gid2, _ := ctp.SelectedGroupPos(); gid2 != gid {
		t.Fatalf("GO 2 (stop) moved selection off the group: group=%d", gid2)
	}
	if awardsSuppressAutoContinue(m1) {
		t.Fatalf("stopped member must not suppress auto-continue")
	}

	if err := FireSelected(); err != nil {
		t.Fatalf("GO 3: %v", err)
	}
	awardsWaitFor(t, "m2 playing", func() bool { return gsp.CurrentPlaying() == "aw2.wav" })
	_ = m2

	if err := FireSelected(); err != nil {
		t.Fatalf("GO 4: %v", err)
	}
	// Last member stopped, no loop: selection steps out and m3 fires.
	awardsWaitFor(t, "continue-out to m3", func() bool {
		pos, _ := ctp.SelectedCuePos()
		return pos == m3 && gsp.CurrentPlaying() == "aw3.wav"
	})
}

// Leaving the group mid-play (arrows) keeps audio ringing but resets the
// session: the next GO on the group starts fresh instead of stopping.
func TestAwardsLeaveResets(t *testing.T) {
	awardsGstAvailable(t)
	gid, m1, _, _ := awardsFixture(t)

	if err := FireSelected(); err != nil {
		t.Fatalf("GO 1: %v", err)
	}
	awardsWaitFor(t, "m1 playing", func() bool { return gsp.CurrentPlaying() == "aw1.wav" })

	// Arrow away: the select endpoint hook must reset the session.
	if err := ctp.SelectStep(1); err != nil {
		t.Fatal(err)
	}
	awardsSelectionSync()
	if awardsSuppressAutoContinue(m1) {
		t.Fatalf("session should reset when the selection leaves the group")
	}
	// Audio keeps ringing.
	if gsp.CurrentPlaying() != "aw1.wav" {
		t.Fatalf("leaving must not stop playback, playing=%q", gsp.CurrentPlaying())
	}

	// Back on the group, GO starts fresh (plays m1 again, not a stop).
	if err := ctp.SetSelectedGroup(gid); err != nil {
		t.Fatal(err)
	}
	awardsSelectionSync()
	if err := FireSelected(); err != nil {
		t.Fatalf("GO after return: %v", err)
	}
	awardsWaitFor(t, "m1 replaying", func() bool { return gsp.CurrentPlaying() == "aw1.wav" })
}

// GO on an empty awards group is a silent no-op (and still owns the
// selection: no advance).
func TestAwardsEmptyGroup(t *testing.T) {
	awardsGstAvailable(t)
	setupTestDB(t)
	gid, err := ctp.CreateGroup("Empty Awards", 0)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := ctp.GetGroup(gid)
	g.AwardsMode = true
	if err := ctp.UpdateGroup(g); err != nil {
		t.Fatal(err)
	}
	gsp.Panic()
	awardsSelectionLeft(0)
	if err := ctp.SetSelectedGroup(gid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gsp.Panic(); awardsSelectionLeft(0) })
	if err := FireSelected(); err != nil {
		t.Fatalf("GO on empty awards group: %v", err)
	}
	if gsp.CurrentPlaying() != "" {
		t.Fatalf("empty awards GO played %q", gsp.CurrentPlaying())
	}
	if gid2, _ := ctp.SelectedGroupPos(); gid2 != gid {
		t.Fatalf("empty awards GO moved selection: group=%d", gid2)
	}
}
