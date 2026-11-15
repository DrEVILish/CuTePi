package gsp

// Audio bus (DESIGN §6.1.2). The HDMI ALSA device takes one stream and its
// dmix cannot produce the device's format, so cues cannot each open the
// device: two running cues with sound would fail. Every cue's (and the
// background playlist's) sound ends in an interaudiosink instead; one bus
// pipeline mixes them all into the configured output:
//
//	interaudiosrc (one per input) -> audiomixer -> audioconvert ->
//	audioresample -> capsfilter (Audio tab rate/channels) -> sink
//
// The bus is built on the first input and released (the device with it)
// once no input has been attached for busIdle.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"
	"golang.org/x/sys/unix"

	"CuTePi/config"
	"CuTePi/gsp/glwall"
	"CuTePi/logs"
)

// busIdle is how long the bus keeps the device after its last input left.
var busIdle = 3 * time.Second

// The bus's delay (DESIGN §6.1.2). The sink's ring buffer is 3 periods of
// 5 ms, its threads run at SCHED_FIFO busRTPrio: on the test Pi with every
// core busy that ran without an xrun (20 ms without the priority had 5-12 a
// minute). The sound leaves no sooner than busLatency after GO, so this
// bounds how soon a cue's first frame can be shown in step with its sound
// (pwStartLatency). alsasink's defaults put the sound ~280 ms late.
const (
	busSinkBuffer     = 15 * time.Millisecond
	busSinkPeriod     = 5 * time.Millisecond
	busMixOutput      = 5 * time.Millisecond // audiomixer output-buffer-duration (its latency is 0)
	busSrcLatency     = 5 * time.Millisecond // interaudiosrc period and latency-time
	busRTPrio         = 8                    // below the presenter's 10 (the unit's LimitRTPRIO)
	busDriftTolerance = 2 * time.Millisecond
	// busLatency is the bus's latency with these settings (its latency
	// query; TestAudioBusLatency). Every input plays that much ahead of its
	// time, so a cue's sound leaves the device with its picture.
	busLatency = 30 * time.Millisecond
)

type busInput struct {
	src *gst.Element
	rs  *gst.Element // audioresample between src and mixer
	pad *gst.Pad     // the mixer's request pad
}

var abus struct {
	mu     sync.Mutex
	p      *gst.Pipeline
	mix    *gst.Element
	sink   *gst.Element
	inputs map[string]*busInput       // channel -> input
	owners map[*gst.Pipeline][]string // pipeline feeding the bus -> its channels
	seq    uint64
	idle   *time.Timer
}

// audioBusCaps is the Audio tab's output format ("" = as negotiated).
func audioBusCaps() string {
	a := config.Audio()
	rate, channels := a.Rate, 0
	if strings.HasPrefix(a.Channels, "2.") {
		channels = 2
	}
	switch {
	case rate > 0 && channels > 0:
		return fmt.Sprintf("audio/x-raw,rate=%d,channels=%d", rate, channels)
	case rate > 0:
		return fmt.Sprintf("audio/x-raw,rate=%d", rate)
	case channels > 0:
		return fmt.Sprintf("audio/x-raw,channels=%d", channels)
	}
	return ""
}

// busStartLocked builds and starts the bus pipeline. Caller holds abus.mu.
func busStartLocked() error {
	if abus.p != nil {
		return nil
	}
	p, err := gst.NewPipeline("cutepi-audio-bus")
	if err != nil {
		return err
	}
	factory := "autoaudiosink"
	if config.Audio().Device != "" {
		factory = "alsasink"
	}
	// Live mixing: inputs are live (interaudiosrc), an input with nothing to
	// say is silence, never a stall. force-live is construct-only.
	mix, err := gst.NewElementWithProperties("audiomixer", map[string]interface{}{"force-live": true})
	if err != nil {
		return err
	}
	rest, err := gst.NewElementMany("audioconvert", "audioresample", "capsfilter", factory)
	if err != nil {
		return err
	}
	els := append([]*gst.Element{mix}, rest...)
	caps, sink := els[3], els[4]
	// The mixer's silence (no input sounding) is flagged GAP, and the sink
	// waits on a GAP until its time plus the whole bus latency, ~200 ms
	// (the device's buffer) later than it takes sound: the first ~200 ms of
	// a cue starting on a silent bus came too late and were lost. As plain
	// silence it is written ahead like any sound.
	mix.GetStaticPad("src").AddProbe(gst.PadProbeTypeBuffer, func(_ *gst.Pad, info *gst.PadProbeInfo) gst.PadProbeReturn {
		if b := info.GetBuffer(); b != nil && b.HasFlags(gst.BufferFlagGap) {
			b.UnsetFlags(gst.BufferFlagGap)
		}
		return gst.PadProbeOK
	})
	mix.Set("ignore-inactive-pads", true)
	mix.Set("output-buffer-duration", uint64(busMixOutput))
	if c := audioBusCaps(); c != "" {
		caps.Set("caps", gst.NewCapsFromString(c))
	}
	if d := config.Audio().Device; d != "" {
		sink.Set("device", d)
	}
	setBuffer := func(e *gst.Element) { // an error: not an audio sink (autoaudiosink's fakesink)
		if e.Set("buffer-time", int64(busSinkBuffer/time.Microsecond)) == nil {
			e.Set("latency-time", int64(busSinkPeriod/time.Microsecond))
			e.Set("processing-deadline", uint64(0)) // 20 ms of latency by default; the ring buffer is the margin
			// The device's clock drifts from the system clock (HDMI: +4.3
			// ppm); the sink skips or repeats samples to stay within ±1 ms
			// of it instead of the default ±20 ms.
			e.Set("drift-tolerance", int64(busDriftTolerance/time.Microsecond))
		}
	}
	if factory == "alsasink" {
		setBuffer(sink)
	} else {
		sink.Connect("element-added", func(_, e any) { // autoaudiosink's device sink
			if el, ok := e.(*gst.Element); ok {
				setBuffer(el)
			}
		})
	}
	if err := p.AddMany(els...); err != nil {
		return err
	}
	if err := gst.ElementLinkMany(els...); err != nil {
		return fmt.Errorf("linking the audio bus: %w", err)
	}
	// The bus runs on the system clock, as the cues and the presenter do:
	// the sink follows it (drift-tolerance). On the device's own clock the
	// channels would drain at the device's rate and the sound drift from
	// the picture without bound. alsasink would provide that clock only
	// once its device is open, which made the choice a race.
	glwall.UseSystemClock(p)
	// Every streaming thread of the bus (sources, mixer, the sink's ring
	// buffer) announces itself on its own thread: raise it there, and lower
	// it when it leaves (pooled threads go on to serve other pipelines).
	p.GetPipelineBus().SetSyncHandler(func(msg *gst.Message) gst.BusSyncReply {
		if msg.Type() == gst.MessageStreamStatus {
			attr := &unix.SchedAttr{Size: unix.SizeofSchedAttr, Policy: unix.SCHED_FIFO, Priority: busRTPrio}
			switch t, _ := msg.ParseStreamStatus(); t {
			case gst.StreamStatusLeave:
				attr.Policy, attr.Priority = unix.SCHED_NORMAL, 0
				fallthrough
			case gst.StreamStatusEnter:
				if err := unix.SchedSetAttr(0, attr, 0); err != nil {
					logs.Printf(logs.GSPPipeDebug, "gsp: audio bus thread priority: %v", err)
				}
			}
		}
		return gst.BusPass
	})
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		if msg.Type() == gst.MessageError {
			e := msg.ParseError()
			logs.PrintfWarn(logs.GSPPipeStopped, "gsp: audio bus: %s: %s (%s)", msg.Source(), e.Error(), e.DebugString())
		}
		return true
	})
	if err := p.SetState(gst.StatePlaying); err != nil {
		p.SetState(gst.StateNull)
		return fmt.Errorf("starting the audio bus: %w", err)
	}
	abus.p, abus.mix, abus.sink = p, mix, sink
	if abus.inputs == nil {
		abus.inputs = map[string]*busInput{}
		abus.owners = map[*gst.Pipeline][]string{}
	}
	return nil
}

// audioBusInput makes the element owner's sound ends in: an interaudiosink
// on a channel of its own, whose interaudiosrc twin feeds the mixer.
func audioBusInput(owner *gst.Pipeline) (*gst.Element, error) {
	sink, err := gst.NewElement("interaudiosink")
	if err != nil {
		return nil, fmt.Errorf("the audio bus needs GStreamer's inter plugin: %w", err)
	}
	if err := audioBusAttach(owner, sink); err != nil {
		return nil, err
	}
	return sink, nil
}

// audioBusAttach connects an interaudiosink owned by owner (a cue pipeline)
// to the bus: a fresh channel, an interaudiosrc reading it, a mixer pad.
func audioBusAttach(owner *gst.Pipeline, sink *gst.Element) error {
	abus.mu.Lock()
	defer abus.mu.Unlock()
	if abus.idle != nil {
		abus.idle.Stop()
		abus.idle = nil
	}
	if err := busStartLocked(); err != nil {
		return err
	}
	abus.seq++
	ch := fmt.Sprintf("cutepi-%d", abus.seq)
	sink.Set("channel", ch)
	in, err := busInputLocked(ch)
	if err != nil {
		return err
	}
	abus.inputs[ch] = in
	abus.owners[owner] = append(abus.owners[owner], ch)
	// Where the sound sits against its picture: the picture is seen the
	// wall's display delay after its time (GPU wall ~115 ms, DESIGN §6.1.1
	// Audio; the plane presenter latches it at its time), the sound leaves
	// busLatency after the input takes it. A positive difference delays the
	// bus (refreshed with the measured delay), a negative one makes the
	// input play early: the presenter's cues start pwStartLatency ahead, so
	// it has the sound in time.
	d := busOffsetLocked()
	sink.Set("ts-offset", int64(min(d, 0)))
	return nil
}

// busOffsetLocked sets the device sink's share of the picture/sound offset
// and returns the whole offset (the inputs take its negative share).
func busOffsetLocked() time.Duration {
	d := -busLatency
	if glOpen {
		d += glwall.DisplayDelay()
	}
	abus.sink.Set("ts-offset", int64(max(d, 0)))
	return d
}

// busInputLocked adds the bus's side of channel ch: an interaudiosrc reading
// it, resampled into a new mixer pad. Caller holds abus.mu.
func busInputLocked(ch string) (*busInput, error) {
	src, err := gst.NewElement("interaudiosrc")
	if err != nil {
		return nil, err
	}
	src.Set("channel", ch)
	// Small periods: the bus adds as little delay to the sound as it can.
	src.Set("period-time", uint64(busSrcLatency))
	src.Set("latency-time", uint64(busSrcLatency))
	// buffer-time only caps the channel's backlog. With alsasink's default
	// buffer the backlog was ~210 ms and at 100 ms it overflowed and was
	// flushed: 93% of a cue's sound was lost (DESIGN §6.1.2). 1 s keeps a
	// margin for a sink with a larger buffer (autoaudiosink's choice).
	src.Set("buffer-time", uint64(time.Second))
	// The mixer takes one rate on every input (it converts format and
	// channels, not rate): a 44.1 kHz cue on a bus running at 48 kHz was
	// refused (not-negotiated) and silent.
	rs, err := gst.NewElement("audioresample")
	if err != nil {
		return nil, err
	}
	if err := abus.p.AddMany(src, rs); err != nil {
		return nil, err
	}
	pad := abus.mix.GetRequestPad("sink_%u")
	if pad == nil {
		abus.p.RemoveMany(src, rs)
		return nil, fmt.Errorf("the audio bus mixer gave no input pad")
	}
	if err := src.Link(rs); err != nil {
		abus.mix.ReleaseRequestPad(pad)
		abus.p.RemoveMany(src, rs)
		return nil, fmt.Errorf("linking an audio bus input: %w", err)
	}
	if ret := rs.GetStaticPad("src").Link(pad); ret != gst.PadLinkOK {
		abus.mix.ReleaseRequestPad(pad)
		abus.p.RemoveMany(src, rs)
		return nil, fmt.Errorf("linking an audio bus input: %v", ret)
	}
	rs.SyncStateWithParent()
	src.SyncStateWithParent()
	return &busInput{src: src, rs: rs, pad: pad}, nil
}

// audioBusRelease disconnects every bus input owner fed (its pipeline is
// being torn down). The bus itself goes after busIdle without inputs.
func audioBusRelease(owner *gst.Pipeline) {
	abus.mu.Lock()
	defer abus.mu.Unlock()
	chs := abus.owners[owner]
	if len(chs) == 0 {
		return
	}
	delete(abus.owners, owner)
	for _, ch := range chs {
		in := abus.inputs[ch]
		delete(abus.inputs, ch)
		if in == nil || abus.p == nil {
			continue
		}
		in.rs.GetStaticPad("src").Unlink(in.pad)
		abus.mix.ReleaseRequestPad(in.pad)
		in.src.SetState(gst.StateNull)
		in.rs.SetState(gst.StateNull)
		abus.p.RemoveMany(in.src, in.rs)
	}
	if len(abus.inputs) == 0 && abus.p != nil && abus.idle == nil {
		abus.idle = time.AfterFunc(busIdle, func() {
			abus.mu.Lock()
			defer abus.mu.Unlock()
			if len(abus.inputs) == 0 {
				busStopLocked()
			}
			abus.idle = nil
		})
	}
}

// audioBusReattach puts a stopped cue's audio sink back on the bus (Stop
// released it); a no-op while it is attached or has no sound.
func audioBusReattach(p *gst.Pipeline) {
	abus.mu.Lock()
	attached := len(abus.owners[p]) > 0
	abus.mu.Unlock()
	if attached {
		return
	}
	if sink, err := p.GetElementByName("cue-audio-sink"); err == nil && sink != nil {
		if err := audioBusAttach(p, sink); err != nil {
			logs.PrintfWarn(logs.GSPPipeDebug, "gsp: audio bus on resume: %v", err)
		}
	}
}

// busStopLocked tears the bus pipeline down (the device is released).
func busStopLocked() {
	if abus.p == nil {
		return
	}
	abus.p.SetState(gst.StateNull)
	abus.p.GetPipelineBus().RemoveWatch() // the watch would keep the bus (and its sockets)
	abus.p, abus.mix, abus.sink = nil, nil, nil
}

// ResetAudioBus rebuilds the bus with the current Audio tab settings (device,
// rate, channels), at once even while cues feed it: their channels are read
// again by the new bus, so running and armed cues keep their sound (a gap
// of the rebuild's length) on the new output. Called after the audio
// settings change.
// shortcut: a cue playing across the change keeps the rebuild's gap as extra
// delay in its channel until refired; drain the channel if that matters.
func ResetAudioBus() {
	abus.mu.Lock()
	defer abus.mu.Unlock()
	busStopLocked()
	if len(abus.inputs) == 0 {
		return
	}
	if err := busStartLocked(); err != nil {
		logs.PrintfWarn(logs.GSPPipeStopped, "gsp: audio bus on the new output: %v", err)
		clear(abus.inputs) // audioBusRelease skips the owners' missing inputs
		return
	}
	for ch := range abus.inputs {
		in, err := busInputLocked(ch)
		if err != nil {
			logs.PrintfWarn(logs.GSPPipeStopped, "gsp: audio bus input %s on the new output: %v", ch, err)
			delete(abus.inputs, ch)
			continue
		}
		abus.inputs[ch] = in
	}
	busOffsetLocked()
}

// AudioBusInputs reports how many inputs feed the bus (tests, debugging).
func AudioBusInputs() int {
	abus.mu.Lock()
	defer abus.mu.Unlock()
	return len(abus.inputs)
}
