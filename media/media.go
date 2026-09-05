// Package media wraps ffmpeg/ffprobe for extracting metadata and
// generating thumbnails/waveforms for uploaded media files.
package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Kind classifies a media file for thumbnailing/UI purposes.
type Kind string

const (
	KindVideo   Kind = "video"
	KindAudio   Kind = "audio"
	KindImage   Kind = "image"
	KindUnknown Kind = "unknown"
)

type Metadata struct {
	Mimetype   string
	Duration   float64
	Resolution string
	Codec      string
	Kind       Kind
}

type ffprobeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type ffprobeFormat struct {
	Duration   string `json:"duration"`
	FormatName string `json:"format_name"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// Probe runs ffprobe on path and extracts duration, resolution, and codec.// It returns an error if ffprobe fails or if duration/codec can't be
// determined - callers should treat that as a failed import per spec.
func Probe(path string) (Metadata, error) {
	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path)
	out, err := cmd.Output()
	if err != nil {
		return Metadata{}, fmt.Errorf("media: ffprobe failed for %q: %w", path, err)
	}

	var parsed ffprobeOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return Metadata{}, fmt.Errorf("media: parsing ffprobe output for %q: %w", path, err)
	}

	var meta Metadata
	duration, err := strconv.ParseFloat(parsed.Format.Duration, 64)
	if err != nil {
		return Metadata{}, fmt.Errorf("media: could not determine duration for %q: %w", path, err)
	}
	meta.Duration = duration

	var videoStream, audioStream *ffprobeStream
	for i, s := range parsed.Streams {
		if s.CodecType == "video" && videoStream == nil {
			videoStream = &parsed.Streams[i]
		}
		if s.CodecType == "audio" && audioStream == nil {
			audioStream = &parsed.Streams[i]
		}
	}

	switch {
	case videoStream != nil && isImageFormat(parsed.Format.FormatName):
		meta.Kind = KindImage
		meta.Codec = videoStream.CodecName
		meta.Resolution = fmt.Sprintf("%dx%d", videoStream.Width, videoStream.Height)
		meta.Mimetype = "image/" + videoStream.CodecName
	case videoStream != nil:
		meta.Kind = KindVideo
		meta.Codec = videoStream.CodecName
		meta.Resolution = fmt.Sprintf("%dx%d", videoStream.Width, videoStream.Height)
		meta.Mimetype = "video/" + parsed.Format.FormatName
	case audioStream != nil:
		meta.Kind = KindAudio
		meta.Codec = audioStream.CodecName
		meta.Resolution = ""
		meta.Mimetype = "audio/" + parsed.Format.FormatName
	default:
		return Metadata{}, fmt.Errorf("media: no usable audio/video stream found in %q", path)
	}

	if meta.Codec == "" {
		return Metadata{}, fmt.Errorf("media: could not determine codec for %q", path)
	}

	return meta, nil
}

// VerifyPlayable runs a short real decode of path and returns an error if the
// file cannot be decoded/corrupt (the import-time playability probe). It
// decodes just enough data to prove the pipeline could play, without writing
// output (-f null discards it). Verified only for video/audio.
func VerifyPlayable(path string) error {
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", path, "-t", "1", "-f", "null", "-")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("media: playability probe failed for %q: %w: %s", path, err, out)
	}
	return nil
}

func isImageFormat(formatName string) bool {
	for _, f := range strings.Split(formatName, ",") {
		switch f {
		case "image2", "png_pipe", "jpeg_pipe", "gif", "webp_pipe", "bmp_pipe":
			return true
		}
	}
	return false
}

// thumbnailSeek picks a safe seek point (in seconds) for grabbing a video
// thumbnail frame: 5s by default, but clamped to the clip's own midpoint
// for anything shorter than 10s so seeking never lands past EOF (which
// makes ffmpeg fail outright rather than just clamping the frame). A
// non-positive or unknown duration falls back to the default.
func thumbnailSeek(duration float64) float64 {
	if duration > 0 && duration < 10 {
		return duration / 2
	}
	return 5.0
}

// GenerateThumbnail creates a 16:9 JPEG thumbnail for the given media file
// at outPath: a frame at 5s (or the clip's midpoint, if shorter than 10s)
// for video, a scaled copy for images, and a waveform image for audio.
// duration is the clip's length in seconds, as reported by Probe; it's
// ignored for images/audio.
func GenerateThumbnail(srcPath string, kind Kind, duration float64, outPath string) error {
	// format=yuvj420p at the end guards against unusual source chroma
	// formats (4:4:4, 10-bit, etc.) that the JPEG/mjpeg encoder can't
	// otherwise take directly.
	const scaleFilter = "scale=640:360:force_original_aspect_ratio=decrease,pad=640:360:(ow-iw)/2:(oh-ih)/2:color=black,format=yuvj420p"

	var cmd *exec.Cmd
	switch kind {
	case KindVideo:
		seek := thumbnailSeek(duration)
		cmd = exec.Command("ffmpeg", "-y", "-ss", fmt.Sprintf("%f", seek), "-i", srcPath, "-frames:v", "1", "-vf", scaleFilter, outPath)
	case KindImage:
		cmd = exec.Command("ffmpeg", "-y", "-i", srcPath, "-frames:v", "1", "-vf", scaleFilter, outPath)
	case KindAudio:
		cmd = exec.Command("ffmpeg", "-y", "-i", srcPath, "-filter_complex",
			"showwavespic=s=640x360:colors=#a855f7", "-frames:v", "1", outPath)
	default:
		return fmt.Errorf("media: unknown kind %q for %q", kind, srcPath)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("media: ffmpeg thumbnail generation failed for %q: %w: %s", srcPath, err, out)
	}
	return nil
}

// GeneratePeaks decodes srcPath's audio to a fixed-bucket amplitude envelope
// (values 0..1, one entry per bucket) suitable for rendering a trim timeline
// waveform. It downmixes to mono float32 and reads it back, so images (no
// audio) yield an all-zero envelope - callers may treat that as "no audio".
func GeneratePeaks(srcPath string) ([]float64, error) {
	const peakBuckets = 300
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", srcPath,
		"-vn", "-ac", "1", "-ar", "100", "-f", "f32le", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("media: opening sample stream for %q: %w", srcPath, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("media: starting analysis for %q: %w", srcPath, err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, out); err != nil {
		return nil, fmt.Errorf("media: reading samples for %q: %w", srcPath, err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("media: waveform analysis failed for %q: %w", srcPath, err)
	}

	sampleCount := buf.Len() / 4
	peaks := make([]float64, peakBuckets)
	if sampleCount == 0 {
		return peaks, nil
	}
	// Evenly spread the (downsampled) samples across the fixed bucket count.
	per := (sampleCount + peakBuckets - 1) / peakBuckets
	raw := buf.Bytes()
	var sample [4]byte
	for i := 0; i < sampleCount; i++ {
		copy(sample[:], raw[i*4:i*4+4])
		v := math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(sample[:]))))
		b := i / per
		if b >= peakBuckets {
			b = peakBuckets - 1
		}
		if v > peaks[b] {
			peaks[b] = v
		}
	}
	// Normalize by the loudest sample so the tallest bar fills the track.
	var max float64
	for _, p := range peaks {
		if p > max {
			max = p
		}
	}
	if max > 0 {
		for i := range peaks {
			peaks[i] /= max
		}
	}
	return peaks, nil
}

// KindFromExtension makes a best-effort guess at a file's media kind from
// its extension, used only for pre-validating uploads before ffprobe runs.
func KindFromExtension(filename string) Kind {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp4", ".mkv", ".mov", ".avi", ".webm", ".m4v":
		return KindVideo
	case ".mp3", ".wav", ".flac", ".aac", ".ogg", ".m4a":
		return KindAudio
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return KindImage
	default:
		return KindUnknown
	}
}
