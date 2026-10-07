package gsp

// Background playlist: a private audio pipeline that plays a list of media
// files sequentially, independent of the main pipeline. The slideshow runner
// uses it as a soundtrack so image slides and music play at the same time
// (the main pipeline can only carry one clip). Any main-pipeline decision —
// Stop, Panic, a new load — kills the music too, so the operator never
// chases a soundtrack around the console.

import (
	"errors"
	"math"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/config"
	"CuTePi/logs"
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

// bgRetryPause is the pause after a track that failed to play, so a bad
// file can never make the playlist spin. A variable for tests.
var bgRetryPause = time.Second

func runPlaylist(files []string, volumeDB float64, done <-chan struct{}, dereg func()) {
	defer dereg()
	// playbin's volume is linear (0..10); cue volume is dB.
	linear := math.Pow(10, volumeDB/20)
	if linear > 10 {
		linear = 10
	}
	for {
		played := false
		for _, f := range files {
			stopped, err := playOneTrack(f, linear, done)
			if stopped {
				return
			}
			if err != nil {
				logs.PrintfWarn(logs.GSPBackground, "background track %q: %v", f, err)
				select {
				case <-done:
					return
				case <-time.After(bgRetryPause):
				}
				continue
			}
			played = true
		}
		// A pass in which nothing played would only fail the same way
		// again: end the soundtrack instead of looping on it.
		if !played {
			logs.PrintfWarn(logs.GSPBackground, "background playlist stopped: none of its %d track(s) could be played", len(files))
			return
		}
	}
}

// fileURI is the file:// URI for path: absolute, with every character a URI
// path cannot carry percent-escaped (pool names may hold spaces, '#', '?'
// or '%'; "file://"+path broke on all of them).
func fileURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String(), nil
}

// playOneTrack builds and plays one audio pipeline, blocking until EOS,
// error, or stop. stopped reports the playlist was stopped; err that the
// track could not be played (setup, state change or a pipeline error).
func playOneTrack(filename string, linear float64, done <-chan struct{}) (stopped bool, err error) {
	uri, err := fileURI(filepath.Join(config.MediaLocation(), filename))
	if err != nil {
		return false, err
	}
	p, err := gst.NewPipeline("cutepi-bgm")
	if err != nil {
		return false, err
	}
	defer p.SetState(gst.StateNull)
	pb, err := gst.NewElement("playbin")
	if err != nil {
		return false, err
	}
	p.Add(pb)
	// The soundtrack's sound goes into the audio bus, mixed with the cues'.
	sink, err := audioBusInput(p)
	if err != nil {
		return false, err
	}
	defer audioBusRelease(p)
	if err := pb.Set("audio-sink", sink); err != nil {
		return false, err
	}
	if err := pb.Set("uri", uri); err != nil {
		return false, err
	}
	if err := pb.Set("volume", linear); err != nil {
		return false, err
	}

	result := make(chan error, 1)
	p.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		switch msg.Type() {
		case gst.MessageEOS:
			result <- nil
			return false
		case gst.MessageError:
			result <- msg.ParseError()
			return false
		}
		return true
	})
	if err := p.SetState(gst.StatePlaying); err != nil {
		return false, err
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return true, nil
		case err := <-result:
			return false, err
		case <-tick.C:
			// Liveness check: a playbin that dropped to NULL without EOS or
			// an error would otherwise hang the playlist forever.
			if _, state := p.GetState(gst.StateNull, 0); state == gst.StateNull {
				return false, errors.New("pipeline stopped without finishing")
			}
		}
	}
}
