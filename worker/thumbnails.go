// Package worker runs background jobs that must survive process restarts by
// keeping their queue state in the database rather than in memory.
package worker

import (
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"CuTePi/config"
	"CuTePi/ctp"
	"CuTePi/media"
)

// RunThumbnailWorker polls for mediapool rows with a pending thumbnail and
// generates them. It runs until the process exits; call it in a goroutine.
// Because "pending" is a DB column rather than an in-memory queue, work
// left over from a crash or restart is picked up automatically on the next
// poll.
func RunThumbnailWorker(pollInterval time.Duration) {
	rebuildOnce.Do(reFlagLowResWaveforms)
	for {
		pending, err := ctp.PendingThumbnails()
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		for _, m := range pending {
			processOne(m)
		}
		time.Sleep(pollInterval)
	}
}

// Envelopes stored before the resolution bump (a fixed small bucket count,
// typically 300 or 2000 regardless of file length) stay blocky when zoomed
// even though new files come out detailed. Flag them once at startup so the
// ordinary pending pipeline regenerates them at the new per-duration
// resolution; a file already matching the target is left untouched (and the
// check is cheap: a comma count, no JSON decode).
func reFlagLowResWaveforms() {
	pool, err := ctp.GetMediapool()
	if err != nil {
		log.Printf("worker: listing media for waveform rebuild: %v", err)
		return
	}
	for _, m := range pool.Medias {
		if m.Waveform != "" && needsWaveformRebuild(m) {
			if err := ctp.RequestWaveformAnalysis(m.Filename); err != nil {
				log.Printf("worker: re-flagging low-res waveform for %q: %v", m.Filename, err)
			}
		}
	}
}

func needsWaveformRebuild(m ctp.Media) bool {
	if m.Waveform == "" || m.Duration <= 0 {
		return false
	}
	if !strings.HasPrefix(strings.TrimSpace(m.Waveform), "[") {
		return true
	}
	bins := strings.Count(m.Waveform, ",") + 1
	target := int(m.Duration * 100) // one bin per ~10ms
	if target < media.MinWaveformBins {
		target = media.MinWaveformBins
	}
	if target > media.MaxWaveformBins {
		target = media.MaxWaveformBins
	}
	return bins < target
}

var rebuildOnce sync.Once

func processOne(m ctp.Media) {
	kind := media.KindFromExtension(m.Filename)
	srcPath := filepath.Join(config.MediaLocation(), m.Filename)
	outPath := filepath.Join(config.ThumbnailLocation(), m.Filename+".jpg")

	if m.ThumbnailPending {
		if err := media.GenerateThumbnail(srcPath, kind, m.Duration, outPath); err != nil {
			// Give up like FailWaveform: a thumbnail that fails once (usually
			// a corrupt/undecodable file) would otherwise stay pending and be
			// re-attempted forever, including on every restart. Operators can
			// re-request regeneration via the refresh button.
			log.Printf("worker: thumbnail generation failed for %q: %v", m.Filename, err)
			_ = ctp.MarkThumbnailDone(m.Media_id)
			return
		}
		if err := ctp.MarkThumbnailDone(m.Media_id); err != nil {
			log.Printf("worker: failed marking thumbnail done for %q: %v", m.Filename, err)
		}
		// Also refresh the detailed codec info (the Media tab): re-probes the
		// file, best-effort - an old meta JSON stays when ffprobe fails.
		if meta, perr := media.Probe(srcPath); perr == nil && meta.Info != nil {
			if raw, jerr := json.Marshal(meta.Info); jerr == nil {
				if err := ctp.UpdateMediaMeta(m.Filename, string(raw)); err != nil {
					log.Printf("worker: storing media meta for %q: %v", m.Filename, err)
				}
			}
		}
	}

	if m.WaveformPending {
		// Keep the pending flag until both analyses finish, so a restart resumes
		// unfinished work. Full-file decoding runs off the import request path.
		// ponytail: serial full-file analysis; split the queue if long files delay thumbnails.
		gain := 0.0
		if kind != media.KindImage {
			var err error
			gain, err = media.MeasureLoudness(srcPath)
			if err != nil {
				log.Printf("worker: loudness analysis for %q: %v", m.Filename, err)
			}
		}
		if err := ctp.StoreLoudnessGain(m.Media_id, gain); err != nil {
			log.Printf("worker: storing loudness for %q: %v", m.Filename, err)
			return
		}
		// Amplitude peaks feed the Cue Inspector's trim timeline. A failure
		// clears the pending flag (so it isn't retried on every poll tick)
		// and leaves the timeline empty - the Analyse button can retry on
		// demand via RequestWaveformAnalysis.
		peaks, perr := media.GeneratePeaks(srcPath)
		if perr != nil {
			log.Printf("worker: waveform analysis failed for %q (flag cleared): %v", m.Filename, perr)
			if err := ctp.FailWaveform(m.Media_id); err != nil {
				log.Printf("worker: failed clearing waveform flag for %q: %v", m.Filename, err)
			}
		} else {
			enc, _ := json.Marshal(peaks)
			if err := ctp.StoreWaveform(m.Media_id, string(enc)); err != nil {
				log.Printf("worker: failed storing waveform for %q: %v", m.Filename, err)
			}
		}
	}
}
