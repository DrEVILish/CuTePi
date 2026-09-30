package routes

// Remote control: the same transport the Web UI drives, over two wire
// protocols show-control networks use to run a deck.
//
//   - HyperDeck Ethernet Protocol (TCP 9993): CuTePi presents itself as a
//     HyperDeck Studio Mini (hyperdeck.go).
//   - QLab OSC (UDP/TCP 53000): the address paths Companion's QLab module
//     sends (qlab.go).
//
// Both halves call the transport cores below.
//
// Neither protocol authenticates, so the operator password does NOT cover
// these listeners: when enabled, anyone who can reach the port can drive
// the transport. They are off by default, and all of them honour the
// configured bind address so they can be pinned to a control-room NIC.
// A bind failure is logged and skipped — losing a control listener must
// never stop the show controller from starting.

import (
	"strconv"

	"CuTePi/ctp"
	"CuTePi/gsp"
	"CuTePi/logs"
)

// FireSelected is the GO action from /cue/selected/play, renderless: fire
// the selected group's playlist (or cue), advance the selection when
// goAdvance is on — except for Awards Mode, which owns its selection and
// never advances (end-of-list steps out on its own). HTTP handlers wrap the
// result in cuesheet HTML; remote protocols have no session to re-render —
// clients pick the change up from WS sync / the cuesheet poller.
func FireSelected() error {
	if gid, gerr := ctp.SelectedGroupPos(); gerr == nil && gid > 0 {
		if g, err := ctp.GetGroup(gid); err == nil {
			if handled := playGroup(g); handled {
				return nil
			}
			if ctp.GetGoAdvance() {
				_ = ctp.SelectStep(1)
			}
			return nil
		}
	}
	pos, err := ctp.SelectedCuePos()
	if err != nil || pos == 0 {
		gsp.Play() // nothing selected: resume the loaded transport
		return nil
	}
	cue, err := ctp.GetCue(strconv.Itoa(pos))
	if err != nil {
		gsp.Play()
		return nil
	}
	if err := loadAndPlayCue(cue); err != nil {
		return err
	}
	// Deck-style preload: after the advance, the newly-selected cue (audio-
	// only) sits prerolled, so the NEXT GO lands instantly instead of paying
	// a build+preroll.
	if ctp.GetGoAdvance() {
		_ = ctp.SelectStep(1)
	}
	if pos, perr := ctp.SelectedCuePos(); perr == nil && pos != 0 {
		armNextCue(gsp.Generation(), pos)
	}
	return nil
}

// FireCue plays cue cuePos without touching the selection, mirroring
// /cue/:cuePos/play including its fade-out-then-start behaviour.
func FireCue(cuePos string) error {
	cue, err := ctp.GetCue(cuePos)
	if err != nil {
		return err
	}
	// With display layers the fade-out of the running cue happens over the
	// new one (cueOpts.Crossfade). Without them, fade it out first, then play.
	if gsp.CurrentPlaying() != "" && cue.FadeOut > 0 && !gsp.Layered() {
		halts, loads := gsp.Halts(), gsp.Loads()
		goSafe(func() {
			gsp.FadeAndStop(cue.FadeOut)
			// The operator stopped/panicked, or another cue took over,
			// while we faded: that newer decision wins.
			if gsp.Halts() != halts || gsp.Loads() != loads {
				return
			}
			if err := loadAndPlayCue(cue); err != nil {
				logs.Printf(logs.RTECuePlay, "queued play failed pos=%d error=%v", cue.CuePos, err)
			}
		})
		return nil
	}
	return loadAndPlayCue(cue)
}

// remotePanic is the PANIC action: panic-hold image when configured, dead
// black otherwise. Shared by the HTTP /panic handler and both protocols
// (QLab's /panic is the same gesture).
func remotePanic() error {
	if hold := ctp.GetPanicHoldImage(); hold != "" {
		// Armed on its own display layer, the image is up within a couple
		// of refreshes; the cold load below takes ~330 ms (panichold.go).
		armed := gsp.PanicToHold(hold)
		kickPanicArm() // re-arm for the next panic
		if armed {
			return nil
		}
		if err := gsp.LoadWithOpts(hold, gsp.LoadOpts{Hold: true}); err == nil {
			return nil
		}
		logs.Printf(logs.RTEPanic, "panic hold image %q unavailable - cutting to black", hold)
	}
	gsp.Panic()
	return nil
}

// StartRemote launches whichever protocol listeners the Settings Network
// tab has enabled (HyperDeck TCP, OSC UDP, OSC-over-TCP/SLIP); all are off
// by default (§12.8).
func StartRemote() {
	ApplyRemote()
}
