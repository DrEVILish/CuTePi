// Package worker runs background jobs that must survive process restarts by
// keeping their queue state in the database rather than in memory.
package worker

import (
	"encoding/json"
	"log"
	"path/filepath"
	"sync"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// retryBackoff is how long to leave a failed item alone before trying its
// thumbnail again, so a persistently-failing file (corrupt upload, missing
// codec) doesn't get re-attempted - and re-log its ffmpeg failure - on
// every single poll tick.
const retryBackoff = 60 * time.Second

// failureTracker remembers when each media item's thumbnail generation last
// failed, so the worker loop can back off retrying it. It's a small,
// dependency-injectable (via the `now` parameter) type specifically so the
// backoff behavior can be unit tested without sleeping in real time.
type failureTracker struct {
	mu   sync.Mutex
	last map[int]time.Time
}

func newFailureTracker() *failureTracker {
	return &failureTracker{last: make(map[int]time.Time)}
}

func (f *failureTracker) shouldSkip(mediaID int, backoff time.Duration, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.last[mediaID]
	return ok && now.Sub(t) < backoff
}

func (f *failureTracker) recordFailure(mediaID int, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[mediaID] = at
}

func (f *failureTracker) clear(mediaID int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.last, mediaID)
}

var failures = newFailureTracker()

// RunThumbnailWorker polls for mediapool rows with a pending thumbnail and
// generates them. It runs until the process exits; call it in a goroutine.
// Because "pending" is a DB column rather than an in-memory queue, work
// left over from a crash or restart is picked up automatically on the next
// poll.
func RunThumbnailWorker(pollInterval time.Duration) {
	for {
		pending, err := ctp.PendingThumbnails()
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		for _, m := range pending {
			if failures.shouldSkip(m.Media_id, retryBackoff, time.Now()) {
				continue
			}
			processOne(m)
		}
		time.Sleep(pollInterval)
	}
}

func processOne(m ctp.Media) {
	kind := media.KindFromExtension(m.Filename)
	srcPath := filepath.Join(config.MediaLocation(), m.Filename)
	outPath := filepath.Join(config.ThumbnailLocation(), m.Filename+".jpg")

	if m.ThumbnailPending {
		if err := media.GenerateThumbnail(srcPath, kind, m.Duration, outPath); err != nil {
			log.Printf("worker: thumbnail generation failed for %q: %v", m.Filename, err)
			failures.recordFailure(m.Media_id, time.Now())
			return
		}
		failures.clear(m.Media_id)
		if err := ctp.MarkThumbnailDone(m.Media_id); err != nil {
			log.Printf("worker: failed marking thumbnail done for %q: %v", m.Filename, err)
		}
	}

	if m.WaveformPending {
		// Amplitude peaks feed the Cue Inspector's trim timeline. A failure
		// just clears the flag (so it isn't retried forever) and leaves the
		// timeline empty - the Analyse button can retry on demand.
		peaks, perr := media.GeneratePeaks(srcPath)
		if perr != nil {
			log.Printf("worker: waveform analysis failed for %q: %v", m.Filename, perr)
		} else {
			enc, _ := json.Marshal(peaks)
			if err := ctp.StoreWaveform(m.Media_id, string(enc)); err != nil {
				log.Printf("worker: failed storing waveform for %q: %v", m.Filename, err)
			}
		}
	}
}
