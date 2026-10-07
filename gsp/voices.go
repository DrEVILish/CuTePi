package gsp

// Simultaneous cues (DESIGN §6.1.2). The manager embeds the focus cue's clip
// (the most recently fired running cue: transport, Now Playing and the remote
// protocols act on it). Every other running cue is a voice: its own pipeline
// on its own display layer, with its own trim, hold, loop and end. When the
// focus goes, the most recently fired voice becomes the focus.

import (
	"sort"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/logs"
)

// Layer placements for a cue that keeps the others running (LoadOpts.Layer).
const (
	LayerTop    = "top"
	LayerBottom = "bottom"
	LayerUnder  = "under"
)

// voice is a running cue other than the focus.
type voice struct {
	clip
}

// clipOfLocked is p's clip, the focus's or a voice's; nil when p is not
// running. Caller holds mu.
func (m *manager) clipOfLocked(p *gst.Pipeline) *clip {
	if p == nil {
		return nil
	}
	if m.pipeline == p {
		return &m.clip
	}
	for _, v := range m.voices {
		if v.pipeline == p {
			return &v.clip
		}
	}
	return nil
}

// runningLocked reports whether p is the focus or a voice. Caller holds mu.
func (m *manager) runningLocked(p *gst.Pipeline) bool { return m.clipOfLocked(p) != nil }

// voiceIndexLocked is p's index in voices, -1 if none. Caller holds mu.
func (m *manager) voiceIndexLocked(p *gst.Pipeline) int {
	for i, v := range m.voices {
		if v.pipeline == p {
			return i
		}
	}
	return -1
}

// startCurrentLocked: may a start in progress for p (begun under generation
// gen) carry on? The focus only if no newer decision was taken (a Stop then
// Play restarts it under a new generation); a cue that was demoted to a
// voice while it started carries on, as the cue joined the stack.
func (m *manager) startCurrentLocked(p *gst.Pipeline, gen uint64) bool {
	c := m.clipOfLocked(p)
	return c != nil && (c != &m.clip || m.gen == gen)
}

// demoteLocked moves the focus into voices (it keeps running). A fade-in
// in progress is finished at once: its ramp belongs to the focus. Caller
// holds mu.
func (m *manager) demoteLocked() {
	v := &voice{clip: m.clip}
	m.voices = append(m.voices, v)
	m.clip = clip{}
}

// promoteLocked makes the most recently fired voice the focus, if any.
// Caller holds mu, with the focus empty.
func (m *manager) promoteLocked() bool {
	if m.pipeline != nil || len(m.voices) == 0 {
		return false
	}
	best := 0
	for i, v := range m.voices {
		if v.fired > m.voices[best].fired {
			best = i
		}
	}
	m.clip = m.voices[best].clip
	m.voices = append(m.voices[:best], m.voices[best+1:]...)
	m.version++
	return true
}

// takeVoicesLocked empties voices and returns their pipelines.
func (m *manager) takeVoicesLocked() []*voice {
	vs := m.voices
	m.voices = nil
	return vs
}

// voiceEnd handles a voice's end (end-of-stream or its trim out-point): loop
// back, hold the last frame, or leave the stack and tell the app layer the
// cue ended (auto-continue, post-wait), as the focus does in handleEnd.
// Returns true while the voice runs on (bus watch kept).
func (m *manager) voiceEnd(p *gst.Pipeline) bool {
	m.mu.Lock()
	i := m.voiceIndexLocked(p)
	if i < 0 {
		m.mu.Unlock()
		return false
	}
	v := m.voices[i]
	if v.loop && !v.liveEndpoint {
		restart, next := loopSteps(v.loopRemain)
		v.loopRemain = next
		if restart {
			inPoint := v.inPoint
			m.mu.Unlock()
			seekAtRate(p, inPoint)
			p.SetState(gst.StatePlaying)
			m.bump()
			return true
		}
	}
	if v.hold && !v.liveEndpoint {
		v.paused, v.heldEnd = true, true
		m.mu.Unlock()
		p.SetState(gst.StatePaused)
		m.bump()
		return true
	}
	pos, cb := v.cuePos, m.onCueEnd
	m.voices = append(m.voices[:i], m.voices[i+1:]...)
	m.version++
	m.mu.Unlock()
	retirePipeline(p)
	broadcastSoon()
	if pos > 0 && cb != nil {
		go cb(pos)
	}
	return false
}

// voiceFailed drops a voice whose pipeline failed (a live page on a lower
// layer included: its recovery runs only while it is the focus).
func (m *manager) voiceFailed(p *gst.Pipeline) {
	m.mu.Lock()
	i := m.voiceIndexLocked(p)
	if i < 0 {
		m.mu.Unlock()
		return
	}
	m.voices = append(m.voices[:i], m.voices[i+1:]...)
	m.version++
	m.mu.Unlock()
	retirePipeline(p)
	broadcastSoon()
}

// StopCue stops the running cue at cuePos alone (Active Cues pane): at once,
// or fading picture and sound out over fadeMs. Other running cues carry on;
// if it was the focus, the most recently fired remaining cue takes over.
// Reports whether such a cue was running.
func StopCue(cuePos int, fadeMs int) bool {
	if cuePos <= 0 {
		return false
	}
	mgr.mu.Lock()
	var c clip
	switch {
	case mgr.pipeline != nil && mgr.cuePos == cuePos:
		c = mgr.clip
		mgr.clip = clip{}
		mgr.gen++
		mgr.promoteLocked()
	default:
		i := -1
		for j, v := range mgr.voices {
			if v.cuePos == cuePos {
				i = j
			}
		}
		if i < 0 {
			mgr.mu.Unlock()
			return false
		}
		c = mgr.voices[i].clip
		mgr.voices = append(mgr.voices[:i], mgr.voices[i+1:]...)
	}
	mgr.version++
	gain, level := c.effectiveGain(), c.fadeLevel
	mgr.mu.Unlock()
	logs.Printf(logs.RTEStop, "stop cue %d alone (fade %d ms)", cuePos, fadeMs)
	if fadeMs > 0 && Layered() {
		o := &outgoing{p: c.pipeline, volumeEl: c.volumeEl, gain: gain, level: level, curve: c.fadeCurve, done: make(chan struct{})}
		go fadeOutgoing(o, fadeMs)
	} else {
		retirePipeline(c.pipeline)
	}
	broadcastSoon()
	return true
}

// Voice is one running cue as the Active Cues pane shows it.
type Voice struct {
	CuePos   int     `json:"cuePos"`
	Title    string  `json:"title"` // the file (or "Live: ...") it plays
	Focus    bool    `json:"focus"` // the transport's cue
	Paused   bool    `json:"paused"`
	Held     bool    `json:"held"`  // parked on its last frame
	Layer    int     `json:"layer"` // stack position, 1 = bottom; 0 = no picture
	Position float64 `json:"position"`
	Duration float64 `json:"duration"`
	Live     bool    `json:"live"`
}

// Voices lists every running cue, top of the stack first (cues without a
// picture last).
func Voices() []Voice {
	mgr.mu.Lock()
	clips := make([]clip, 0, len(mgr.voices)+1)
	if mgr.pipeline != nil && !mgr.testShowing {
		clips = append(clips, mgr.clip)
	}
	for _, v := range mgr.voices {
		clips = append(clips, v.clip)
	}
	focus := mgr.pipeline
	mgr.mu.Unlock()
	out := make([]Voice, 0, len(clips))
	for _, c := range clips {
		v := Voice{CuePos: c.cuePos, Title: c.currentFile, Focus: c.pipeline == focus, Paused: c.paused,
			Held: c.heldEnd, Layer: stackPosition(c.pipeline), Live: c.liveEndpoint}
		if !c.liveEndpoint {
			// Position and duration outside mu (queries can block on the bus).
			if ok, pos := c.pipeline.QueryPosition(gst.FormatTime); ok {
				v.Position = float64(pos) / 1e9
			}
			if ok, dur := c.pipeline.QueryDuration(gst.FormatTime); ok {
				v.Duration = float64(dur) / 1e9
			}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Layer == 0) != (out[j].Layer == 0) {
			return out[j].Layer == 0
		}
		return out[i].Layer > out[j].Layer
	})
	return out
}

// RunningCues lists the cue positions of every running cue.
func RunningCues() []int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	var out []int
	if mgr.pipeline != nil && mgr.cuePos > 0 {
		out = append(out, mgr.cuePos)
	}
	for _, v := range mgr.voices {
		if v.cuePos > 0 {
			out = append(out, v.cuePos)
		}
	}
	return out
}

// stackable reports whether cues can run side by side: on a layered wall
// (one display layer each), or on a sink with no single output (fakesink,
// the tests). The framebuffer fallback has one picture: a cue there always
// replaces the running one.
func stackable() bool {
	return Layered() || wallVideoSink() == "fakesink"
}
