package gsp

import (
	"CuTePi/gsp/planewall"
	"math"
	"path/filepath"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/config"
	"CuTePi/media"
)

// levelRamp is a fade of the picture level: from -> to over dur along curve,
// started at start.
type levelRamp struct {
	from, to float64
	start    time.Time
	dur      time.Duration
	curve    string
}

// Caller holds mu; live changes and both fades share the same effective gain.
func (m *clip) effectiveGain() float64 {
	if m.mute || m.volume <= -60 {
		return 0
	}
	return dbToGain(dbClamp(m.volume+m.loudnessGain)) * m.fadeLevel
}

// applyBrightness drives the video fade level. A still is a single buffer
// the sink has already shown, so a new brightness would never reach the
// screen: re-render it. Caller holds mu.
// applyLayerLevel applies a voice's fade level to its layer (voices only run
// on layered walls). Caller holds mgr.mu.
func (c *clip) applyLayerLevel() {
	if r := c.fadeRamp; r != nil {
		setLayerRamp(c.pipeline, r.from, r.to, r.start, r.dur, r.curve)
	} else {
		setLayerLevel(c.pipeline, c.fadeLevel)
	}
}

func (m *manager) applyBrightness() {
	if kmsWall() != nil {
		// Plane alpha over the black primary: a true fade (colours scale,
		// never shift), no re-render, applied at the next vblank. A fade in
		// progress goes to the wall whole (the GPU wall steps it per frame).
		if r := m.fadeRamp; r != nil {
			setLayerRamp(m.pipeline, r.from, r.to, r.start, r.dur, r.curve)
		} else {
			setLayerLevel(m.pipeline, m.fadeLevel)
		}
		return
	}
	if m.brightEl == nil {
		return
	}
	if !m.still {
		if !m.brightBusy {
			m.brightBusy = true
			go m.brightWorker()
		}
		return
	}
	m.brightEl.Set("brightness", m.fadeLevel-1)
	if m.pipeline == nil {
		return
	}
	m.stillDirty = true
	if m.stillBusy {
		return
	}
	m.stillBusy = true
	go m.rerenderStill(m.pipeline)
}

// brightWorker applies the latest fade level to the video branch off the fade
// clock. Setting videobalance's brightness blocks on the element's stream lock
// for as long as a frame is being pushed downstream (hundreds of ms on a 1080p
// clip running ahead of the sink), and doing it inline under mu stretched a
// 500ms ESC fade to ~2s and stalled every status reader behind it. The audio
// ramp and the fade deadline stay on time; video follows as fast as frames flow.
func (m *manager) brightWorker() {
	var lastEl *gst.Element
	var last float64
	for {
		m.mu.Lock()
		el, v := m.brightEl, m.fadeLevel-1
		if el == nil || (el == lastEl && v == last) {
			m.brightBusy = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		el.Set("brightness", v)
		lastEl, last = el, v
	}
}

// rerenderStill flush-seeks a still back to its only frame whenever the fade
// level changed, paced by how fast the frame decodes. The seek makes the
// source emit its frame (and an EOS) again; stillBusy keeps that echo from
// counting as the cue ending a second time.
func (m *manager) rerenderStill(p *gst.Pipeline) {
	for {
		m.mu.Lock()
		if !m.stillDirty || m.pipeline != p {
			m.stillDirty = false
			m.mu.Unlock()
			time.Sleep(150 * time.Millisecond) // let the last echo EOS land
			m.mu.Lock()
			if !m.stillDirty || m.pipeline != p {
				m.stillBusy = false
				m.mu.Unlock()
				return
			}
		}
		m.stillDirty = false
		m.mu.Unlock()
		p.SeekSimple(0, gst.FormatTime, gst.SeekFlagFlush|gst.SeekFlagAccurate)
		// Wait for the sink to preroll the frame: a new flush before the
		// decode finishes cancels it, and a ramp faster than the decode
		// time would show nothing until the last step.
		p.GetState(gst.StateNull, gst.ClockTime(time.Second))
	}
}

// isStillFile reports whether name is a single-frame image (the same
// extension list import uses, media.KindFromExtension). An animated GIF,
// APNG or WebP is not a still: it plays as a timeline, so none of the
// single-frame shortcuts (re-render on fades, infinite hold, armed panic
// image) apply to it (§6.1.3).
func isStillFile(name string) bool {
	return media.KindFromExtension(name) == media.KindImage && !animationOf(name).Animated
}

// animationOf reads the animation header of a media-pool file (microseconds:
// header only).
func animationOf(name string) media.Animation {
	if media.KindFromExtension(name) != media.KindImage {
		return media.Animation{}
	}
	return media.ImageAnimation(filepath.Join(config.MediaLocation(), name))
}

// IsAnimated reports whether filename is an animated image (GIF, APNG, WebP).
func IsAnimated(filename string) bool { return animationOf(filename).Animated }

func (m *clip) applyGain() {
	if m.volumeEl != nil {
		m.volumeEl.Set("volume", m.effectiveGain())
	}
}

// fadeShape maps ramp progress t (0..1) through the selected envelope curve.
// All callers share one envelope (audio gain + video brightness), so a curve
// changes both identically. linear is the historical flat ramp.
func fadeShape(curve string, t float64) float64 {
	if t <= 0 {
		return 0
	}
	if t >= 1 {
		return 1
	}
	switch curve {
	case "smooth": // S-curve (smoothstep): slow ends, fast middle
		return t * t * (3 - 2*t)
	case "log": // perceptual: fast initial change, long tail
		return math.Log10(1+9*t) / 1
	case "exp": // slow start, sharp finish
		const k = 3.0
		return (math.Exp(k*t) - 1) / (math.Exp(k) - 1)
	default: // linear
		return t
	}
}

func fadeIn(p *gst.Pipeline, gen uint64) {
	mgr.mu.Lock()
	c0 := mgr.clipOfLocked(p)
	if c0 == nil {
		mgr.mu.Unlock()
		return
	}
	serial, duration, live, still := c0.fadeSerial, time.Duration(c0.fadeIn)*time.Millisecond, c0.liveEndpoint, c0.still
	mgr.mu.Unlock()
	if duration <= 0 {
		return
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var elapsed time.Duration
	last := time.Now()
	if planewall.IsOpen() && !live && !still {
		// Running time zero is the first frame's presentation time. Do not
		// consume opening media while waiting for extra frames to arrive.
		last = dueTime(p)
	}
	// The first step at once: a cue shown at level 0 would otherwise wait a
	// tick for its first light.
	for now := time.Now(); ; now = <-ticker.C {
		_, state := p.GetState(gst.StateNull, 0)
		delta := max(time.Duration(0), now.Sub(last))
		if now.After(last) {
			last = now
		}
		onWall := !glOpen || glOnWall(p)
		mgr.mu.Lock()
		// The focus, or the voice it became when a cue that keeps the
		// others running was fired (§6.1.2): the fade carries on there. A
		// newer fade (a restart, ESC) bumps the serial.
		c := mgr.clipOfLocked(p)
		if c == nil || c.fadeSerial != serial {
			mgr.mu.Unlock()
			return
		}
		// A still ends (and holds, paused) the moment its one frame is
		// out, so its fade clock cannot wait for PLAYING.
		// On the GPU wall the clock also waits for the layer to be shown.
		// A live page is shown only once its frames are on screen
		// (showLive), maybe before its pipeline reports PLAYING.
		advancing := (state == gst.StatePlaying || c.still || live) && onWall
		if advancing {
			elapsed += delta
		}
		c.fadeLevel = fadeShape(c.fadeCurve, math.Min(1, float64(elapsed)/float64(duration)))
		if advancing {
			// Started `elapsed` ago: the wall steps the rest per frame. A
			// held fade (paused, not on the wall yet) is a plain level.
			c.fadeRamp = &levelRamp{from: 0, to: 1, start: now.Add(-elapsed), dur: duration, curve: c.fadeCurve}
		}
		c.applyGain()
		if c == &mgr.clip {
			mgr.applyBrightness()
		} else {
			c.applyLayerLevel()
		}
		c.fadeRamp = nil
		done := c.fadeLevel == 1
		mgr.mu.Unlock()
		if done {
			return
		}
	}
}

// dbToGain converts a dB value (as stored per-cue) to the linear gain the
// GStreamer "volume" element expects: 0dB -> 1.0, -6dB -> 0.5, +6dB -> 2.0.
// Values at or below -inf-ish dB clamp to silence.
func dbToGain(db float64) float64 {
	if db <= -60 {
		return 0
	}
	return math.Pow(10, db/20.0)
}

// dbClamp clamps a dB value to the Cue Inspector slider's range.
func dbClamp(v float64) float64 {
	return math.Max(-60, math.Min(12, v))
}

func clampRate(rate float64) float64 {
	if rate == 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 1
	}
	return math.Max(0.25, math.Min(4, rate))
}

func SetRate(rate float64) {
	mgr.mu.Lock()
	rate = clampRate(rate)
	if mgr.rate == rate {
		mgr.mu.Unlock()
		return
	}
	mgr.rate = rate
	p, starting := mgr.pipeline, mgr.starting
	mgr.mu.Unlock()
	if p != nil && !starting {
		// A preceding flush seek may still be prerolling; its position query
		// is temporarily unavailable. Wait before issuing the new rate seek.
		p.GetState(gst.StateNull, gst.ClockTime(5*time.Second))
		if ok, pos := p.QueryPosition(gst.FormatTime); ok {
			seekAtRate(p, float64(pos)/1e9)
		}
	}
}

// SetCueVolume applies a live gain (dB) to the playing pipeline and reports
// the clamped value and the cue position that pipeline plays, read under
// the same lock. The caller persists the value on exactly that cue: reading
// CurrentCuePos separately could save it to a cue fired in between (0 when
// the clip has no cue).
func SetCueVolume(v float64) (applied float64, cuePos int) {
	mgr.mu.Lock()
	if !math.IsNaN(v) {
		mgr.volume = dbClamp(v)
		mgr.applyGain()
	}
	applied, cuePos = mgr.volume, mgr.cuePos
	mgr.mu.Unlock()
	mgr.bump()
	return applied, cuePos
}

// CueMix is a cue's live mix: what the Cue Inspector re-applies after a
// save while the cue plays.
type CueMix struct {
	Mute    bool
	Volume  float64 // dB
	Balance float64
	Rate    float64
}

// ApplyCueMix applies mix to the playing pipeline only if it still plays
// cuePos, checked under the lock that applies it, so a cue fired meanwhile
// never receives another cue's settings. Reports whether it applied.
func ApplyCueMix(cuePos int, mix CueMix) bool {
	mgr.mu.Lock()
	if cuePos <= 0 || mgr.cuePos != cuePos || mgr.pipeline == nil {
		mgr.mu.Unlock()
		return false
	}
	mgr.mute = mix.Mute
	if !math.IsNaN(mix.Volume) {
		mgr.volume = dbClamp(mix.Volume)
	}
	mgr.applyGain()
	if !math.IsNaN(mix.Balance) && !math.IsInf(mix.Balance, 0) {
		mgr.balance = math.Max(-1, math.Min(1, mix.Balance))
		if mgr.panEl != nil {
			mgr.panEl.Set("panorama", float32(mgr.balance))
		}
	}
	rate := clampRate(mix.Rate)
	rateChanged := mgr.rate != rate
	mgr.rate = rate
	p, starting := mgr.pipeline, mgr.starting
	mgr.mu.Unlock()
	mgr.bump()
	if rateChanged && !starting {
		// As SetRate; seekAtRate skips the seek if p was replaced meanwhile.
		p.GetState(gst.StateNull, gst.ClockTime(5*time.Second))
		if ok, pos := p.QueryPosition(gst.FormatTime); ok {
			seekAtRate(p, float64(pos)/1e9)
		}
	}
	return true
}
