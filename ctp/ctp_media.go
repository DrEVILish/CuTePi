package ctp

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"time"

	"CuTePi/media"
)

// RegisterMedia inserts a newly-uploaded file (already saved to the media
// directory as filename) into the mediapool, using the given probed
// metadata. The thumbnail is left pending for the background worker.
// ponytail: the `title` param is kept for API stability but no longer
// persisted - nothing ever read media_title; restore the column write (the
// DB column stays, nullable) if a surface for it appears.
func RegisterMedia(filename string, size int64, meta media.Metadata, title string) (err error) {
	// Upsert on filename: re-uploading a file the pool already knows replaces
	// its probe metadata and re-queues background work while keeping media_id
	// (and so every cue's FK to it) stable. A plain INSERT here made any
	// re-upload of a registered filename fail with a UNIQUE constraint error,
	// breaking the operator's "replace a file" flow.
	metaJSON, merr := json.Marshal(meta.Info)
	metaInfo := ""
	if merr == nil && meta.Info != nil {
		metaInfo = string(metaJSON)
	}
	_, err = db.Exec(`
		INSERT INTO mediapool (filename, mimetype, size, duration, resolution, media_meta, thumbnail_pending, waveform_pending)
		VALUES (:filename, :mimetype, :size, :duration, :resolution, :media_meta, 1, 1)
		ON CONFLICT(filename) DO UPDATE SET
			mimetype = excluded.mimetype,
			size = excluded.size,
			duration = excluded.duration,
			resolution = excluded.resolution,
			media_meta = excluded.media_meta,
			thumbnail_pending = 1,
			loudness_gain = 0,
			missing = 0,
			waveform_pending = 1;
	`,
		sql.Named("filename", filename),
		sql.Named("mimetype", meta.Mimetype),
		sql.Named("size", size),
		sql.Named("duration", meta.Duration),
		sql.Named("resolution", meta.Resolution),
		sql.Named("media_meta", metaInfo),
	)
	if err != nil {
		log.Printf("Error registering uploaded file: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// PendingThumbnails returns media rows still awaiting background work
// (thumbnail and/or waveform generation), so a background worker can pick
// them up (including after a restart, since the pending flags are persisted
// in the DB rather than an in-memory queue).
func PendingThumbnails() (medias []Media, err error) {
	err = db.Select(&medias, `SELECT * FROM mediapool WHERE source_kind = 'file' AND (thumbnail_pending = 1 OR waveform_pending = 1)`)
	if err != nil {
		log.Printf("Error listing pending media work: %v", err)
		return nil, err
	}
	return medias, nil
}

// MarkThumbnailDone clears the pending flag for a media file, e.g. after
// its thumbnail has been (re)generated.
func MarkThumbnailDone(mediaID int) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET thumbnail_pending = 0 WHERE media_id = ?;`, mediaID)
	if err != nil {
		log.Printf("Error marking thumbnail done: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// RequestThumbnailRefresh flags a media file's thumbnail for regeneration.
func RequestThumbnailRefresh(filename string) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET thumbnail_pending = 1 WHERE filename = ?;`, filename)
	if err != nil {
		log.Printf("Error requesting thumbnail refresh: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// MediaHasAlpha reports whether a registered file's video carries an alpha
// channel, from the metadata recorded at import (false when unknown).
func MediaHasAlpha(filename string) bool {
	var meta string
	if db.Get(&meta, `SELECT media_meta FROM mediapool WHERE filename = ?`, filename) != nil || meta == "" {
		return false
	}
	var info media.MediaInfo
	if json.Unmarshal([]byte(meta), &info) != nil {
		return false
	}
	return info.HasAlpha()
}

// MediaFieldOrder is a media file's field order as probed at import (ffprobe:
// "tt", "bb", "tb", "bt"), "" for progressive or unknown video. Files
// imported before the field order was kept report "i" when they were
// recorded as interlaced.
func MediaFieldOrder(filename string) string {
	var meta string
	if db.Get(&meta, `SELECT media_meta FROM mediapool WHERE filename = ?`, filename) != nil || meta == "" {
		return ""
	}
	var info media.MediaInfo
	if json.Unmarshal([]byte(meta), &info) != nil || info.Video == nil || !info.Video.Interlaced {
		return ""
	}
	if info.Video.FieldOrder != "" {
		return info.Video.FieldOrder
	}
	return "i"
}

// MediaVideoInfo is filename's probed video stream (nil without one).
func MediaVideoInfo(filename string) *media.MediaVideoInfo {
	var meta string
	if db.Get(&meta, `SELECT media_meta FROM mediapool WHERE filename = ?`, filename) != nil || meta == "" {
		return nil
	}
	var info media.MediaInfo
	if json.Unmarshal([]byte(meta), &info) != nil {
		return nil
	}
	return info.Video
}

// MediaVideoCodec is a media file's video codec and pixel format as probed
// at import ("hevc/yuv420p", "h264/yuv420p10le", ...), "" if unknown.
func MediaVideoCodec(filename string) string {
	var meta string
	if db.Get(&meta, `SELECT media_meta FROM mediapool WHERE filename = ?`, filename) != nil || meta == "" {
		return ""
	}
	var info media.MediaInfo
	if json.Unmarshal([]byte(meta), &info) != nil || info.Video == nil {
		return ""
	}
	return info.Video.Codec + "/" + info.Video.PixFmt
}

func MediaLoudnessGain(filename string) (gain float64, err error) {
	err = db.Get(&gain, `SELECT loudness_gain FROM mediapool WHERE filename = ?`, filename)
	return
}

func StoreLoudnessGain(mediaID int, gain float64) error {
	if math.IsNaN(gain) || math.IsInf(gain, 0) {
		return fmt.Errorf("invalid loudness gain")
	}
	_, err := db.Exec(`UPDATE mediapool SET loudness_gain = ? WHERE media_id = ?`, gain, mediaID)
	return err
}

// StoreWaveform persists the computed amplitude peaks (JSON) for a media
// file and clears its waveform-pending flag.
func StoreWaveform(mediaID int, peaksJSON string) (err error) {
	_, err = db.Exec(`INSERT INTO media_waveform (media_id, peaks, updated) VALUES (?, ?, ?)
		ON CONFLICT(media_id) DO UPDATE SET peaks = excluded.peaks, updated = excluded.updated`,
		mediaID, peaksJSON, time.Now().UnixMilli())
	if err == nil {
		_, err = db.Exec(`UPDATE mediapool SET waveform_pending = 0 WHERE media_id = ?;`, mediaID)
	}
	if err != nil {
		log.Printf("Error storing waveform: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// FailWaveform gives up on a waveform that cannot be computed (undecodable
// file, missing codec): clears the pending flag so the worker stops
// retrying it every poll tick, leaving the timeline empty. The Analyse
// button can retry on demand via RequestWaveformAnalysis.
func FailWaveform(mediaID int) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET waveform_pending = 0 WHERE media_id = ?;`, mediaID)
	if err != nil {
		log.Printf("Error clearing waveform flag: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

// RequestWaveformAnalysis flags a media file for (re)analysis by the
// background worker. "Analyse" regenerates the amplitude peaks used by the
// Cue Inspector's trim timeline.
func RequestWaveformAnalysis(filename string) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET waveform_pending = 1 WHERE filename = ?;`, filename)
	if err != nil {
		log.Printf("Error requesting waveform analysis: %v", err)
		return err
	}
	bumpMediaVersion()
	return nil
}

func Delete(filename string) (err error) {
	res, err := db.Exec(`
		DELETE FROM mediapool
		WHERE filename = :filename;
	`, sql.Named("filename", filename))
	if err != nil {
		log.Printf("Error deleting cue from mediapool: %v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("media %q not found in pool", filename)
	}
	// A deleted file must not linger as a pinned test pattern or as the
	// panic holding image (both would point at nothing).
	if err := RemoveTestPattern(filename); err != nil {
		log.Printf("Error unpinning deleted media %q from test patterns: %v", filename, err)
	}
	if GetPanicHoldImage() == filename {
		if err := SetPanicHoldImage(""); err != nil {
			log.Printf("Error clearing deleted panic holding image %q: %v", filename, err)
		}
	}
	bumpMediaVersion()
	bumpCuesheetVersion()
	return nil
}

// UpdateMediaMeta persists refreshed detailed media info (JSON MediaInfo)
// for an already-registered media file.
func UpdateMediaMeta(filename string, metaJSON string) (err error) {
	_, err = db.Exec(`UPDATE mediapool SET media_meta = ? WHERE filename = ?;`, metaJSON, filename)
	if err != nil {
		return err
	}
	bumpMediaVersion()
	return nil
}
