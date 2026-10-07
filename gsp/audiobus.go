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

	"CuTePi/config"
	"CuTePi/gsp/glwall"
	"CuTePi/logs"
)

// busIdle is how long the bus keeps the device after its last input left.
var busIdle = 3 * time.Second

type busInput struct {
	src *gst.Element
	pad *gst.Pad // the mixer's request pad
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
	mix.Set("ignore-inactive-pads", true)
	mix.Set("latency", uint64(20*time.Millisecond))
	if c := audioBusCaps(); c != "" {
		caps.Set("caps", gst.NewCapsFromString(c))
	}
	if d := config.Audio().Device; d != "" {
		sink.Set("device", d)
	}
	if glOpen {
		// The wall shows a frame ~115 ms after its time; the sound waits as
		// long (DESIGN §6.1.1, Audio).
		glwall.AlignAudio(sink)
	}
	if err := p.AddMany(els...); err != nil {
		return err
	}
	if err := gst.ElementLinkMany(els...); err != nil {
		return fmt.Errorf("linking the audio bus: %w", err)
	}
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		if msg.Type() == gst.MessageError {
			logs.PrintfWarn(logs.GSPPipeStopped, "gsp: audio bus: %s", msg.ParseError().Error())
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
	src, err := gst.NewElement("interaudiosrc")
	if err != nil {
		return err
	}
	src.Set("channel", ch)
	// Small periods: the bus adds as little delay to the sound as it can.
	src.Set("period-time", uint64(10*time.Millisecond))
	src.Set("latency-time", uint64(20*time.Millisecond))
	src.Set("buffer-time", uint64(100*time.Millisecond))
	if err := abus.p.AddMany(src); err != nil {
		return err
	}
	pad := abus.mix.GetRequestPad("sink_%u")
	if pad == nil {
		abus.p.Remove(src)
		return fmt.Errorf("the audio bus mixer gave no input pad")
	}
	if ret := src.GetStaticPad("src").Link(pad); ret != gst.PadLinkOK {
		abus.mix.ReleaseRequestPad(pad)
		abus.p.Remove(src)
		return fmt.Errorf("linking an audio bus input: %v", ret)
	}
	src.SyncStateWithParent()
	abus.inputs[ch] = &busInput{src: src, pad: pad}
	abus.owners[owner] = append(abus.owners[owner], ch)
	if glOpen {
		glwall.AlignAudio(abus.sink) // the measured display delay, refreshed
	}
	return nil
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
		in.src.GetStaticPad("src").Unlink(in.pad)
		abus.mix.ReleaseRequestPad(in.pad)
		in.src.SetState(gst.StateNull)
		abus.p.Remove(in.src)
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
	abus.p, abus.mix, abus.sink = nil, nil, nil
}

// ResetAudioBus rebuilds the bus with the current Audio tab settings on its
// next use (the running cues keep their sound only once they are fired
// again). Called after the audio settings change.
func ResetAudioBus() {
	abus.mu.Lock()
	defer abus.mu.Unlock()
	if len(abus.inputs) == 0 {
		busStopLocked()
	}
}

// AudioBusInputs reports how many inputs feed the bus (tests, debugging).
func AudioBusInputs() int {
	abus.mu.Lock()
	defer abus.mu.Unlock()
	return len(abus.inputs)
}
