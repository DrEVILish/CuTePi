package gsp

// Background playlist: a private audio pipeline that plays a list of media
// files sequentially, independent of the main pipeline. The slideshow runner
// uses it as a soundtrack so image slides and music play at the same time
// (the main pipeline can only carry one clip). Any main-pipeline decision —
// Stop, Panic, a new load — kills the music too, so the operator never
// chases a soundtrack around the console.

import (
	"math"
	"os"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/config"
)

var (
	bgMu   sync.Mutex
	bgStop func()
)

// setBGStop registers the active background playlist's stop func.
func setBGStop(f func()) {
	bgMu.Lock()
	bgStop = f
	bgMu.Unlock()
}

// stopBackground tears down any active background playlist. Called from the
// main pipeline's decision points (Stop/Panic/swap). stopBg closures never
// touch mgr state, so this cannot deadlock against mgr.mu.
func stopBackground() {
	bgMu.Lock()
	f := bgStop
	bgStop = nil
	bgMu.Unlock()
	if f != nil {
		f()
	}
}

// BackgroundPlaylist plays files (media-dir-relative, same convention as
// Load) one after another on a private audio pipeline, looping the list
// until stopped. volumeDB is the soundtrack gain (cue volume semantics:
// 0 dB = unity). Returns the stop func; calling it twice is safe, and it
// is also invoked automatically by Stop/Panic/swap.
func BackgroundPlaylist(files []string, volumeDB float64) (stop func()) {
	gstInit()
	if len(files) == 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	stopFn := func() { once.Do(func() { close(done) }) }
	setBGStop(stopFn)
	go runPlaylist(files, volumeDB, done, stopFn)
	return stopFn
}

func runPlaylist(files []string, volumeDB float64, done <-chan struct{}, dereg func()) {
	defer dereg()
	// playbin's volume is linear (0..10); cue volume is dB.
	linear := math.Pow(10, volumeDB/20)
	if linear > 10 {
		linear = 10
	}
	for {
		for _, f := range files {
			if !playOneTrack(f, linear, done) {
				return // stopped
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}
}

// playOneTrack builds and plays one audio pipeline, blocking until EOS,
// error, or stop. Returns false only when the playlist was stopped.
func playOneTrack(filename string, linear float64, done <-chan struct{}) bool {
	path := config.MediaLocation() + string(os.PathSeparator) + filename
	p, err := gst.NewPipeline("cutepi-bgm")
	if err != nil {
		return true
	}
	pb, err := gst.NewElement("playbin")
	if err != nil {
		return true
	}
	p.Add(pb)
	_ = pb.Set("uri", "file://"+path)
	_ = pb.Set("volume", linear)

	eos := make(chan struct{})
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			close(eos)
			return false
		case gst.MessageError:
			close(eos)
			return false
		}
		return true
	})
	if p.SetState(gst.StatePlaying) != nil {
		return true
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			_ = p.SetState(gst.StateNull)
			return false
		case <-eos:
			_ = p.SetState(gst.StateNull)
			return true
		case <-tick.C:
			// Liveness check: a playbin that never reaches EOS or error
			// (broken file) would otherwise hang the playlist forever.
			_, state := p.GetState(gst.StateNull, 0)
			if state == gst.StateNull {
				return true
			}
		}
	}
}
