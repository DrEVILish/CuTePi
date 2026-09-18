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

// Waveform envelope resolution bounds. GeneratePeaks targets ~10ms per
// amplitude bin (up to a cap); the worker compares stored envelopes against
// these same bounds to spot pre-bump low-res archives and rebuild them.
// ponytail: one resolution per file suits medium orchestration cues; a full
// second-level pyramid (or on-demand window probes) is the upgrade path if a
// show routinely needs to zoom into minute-long files.
const (
	MinWaveformBins = 500
	MaxWaveformBins = 16000
)

type Metadata struct {
	Mimetype   string
	Duration   float64
	Resolution string
	Codec      string
	Kind       Kind
	Info       *MediaInfo
}

type ffprobeStream struct {
	CodecType      string `json:"codec_type"`
	CodecName      string `json:"codec_name"`
	Profile        string `json:"profile"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	PixFmt         string `json:"pix_fmt"`
	ColorSpace     string `json:"color_space"`
	ColorTransfer  string `json:"color_transfer"`
	ColorPrimaries string `json:"color_primaries"`
	AvgFrameRate   string `json:"avg_frame_rate"`
	BitRate        string `json:"bit_rate"`
	Channels       int    `json:"channels"`
	ChannelLayout  string `json:"channel_layout"`
	SampleRate     string `json:"sample_rate"`
}

type ffprobeFormat struct {
	Duration   string `json:"duration"`
	FormatName string `json:"format_name"`
	BitRate    string `json:"bit_rate"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// MediaInfo carries the detailed codec/container data rendered by the Cue
// Inspector's Media tab (mirrors ffprobe/vlc codec-info views). Nil pointer
// sections mean "no such stream".
type MediaVideoInfo struct {
	Codec   string
	Profile string
	Width   int
	Height  int
	FPS     float64
	PixFmt  string
	Bitrate int
	Color   string
}
type MediaAudioInfo struct {
	Codec      string
	Layout     string
	Channels   int
	SampleRate int
	Bitrate    int
}
type MediaInfo struct {
	Container      string
	OverallBitrate int
	Duration       float64
	Video          *MediaVideoInfo
	Audio          *MediaAudioInfo
}

// ratio parses a "num/den" or plain number string (0 if unparseable).
func ratio(s string) float64 {
	if s == "" {
		return 0
	}
	if i := strings.Index(s, "/"); i > 0 {
		n, err1 := strconv.Atoi(s[:i])
		d, err2 := strconv.Atoi(s[i+1:])
		if err1 != nil || err2 != nil || d == 0 {
			return 0
		}
		return float64(n) / float64(d)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// num parses a decimal string that ffprobe may replace with "N/A" (0 if so).
func num(s string) int {
	if s == "" || strings.ContainsAny(s, "NA ") {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		// some bitrates carry a fraction
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return 0
		}
		return int(f)
	}
	return v
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

	// Detailed codec/container info for the inspector's Media tab. Best-effort:
	// probe fields ffprobe omits (e.g. "N/A" bitrates on some containers) are
	// zeroed out, never fatal.
	if videoStream != nil {
		cs := videoStream.ColorPrimaries
		if cs == "" {
			cs = videoStream.ColorSpace
		}
		meta.Info = &MediaInfo{
			Container:      parsed.Format.FormatName,
			OverallBitrate: num(parsed.Format.BitRate),
			Video: &MediaVideoInfo{
				Codec:   videoStream.CodecName,
				Profile: videoStream.Profile,
				Width:   videoStream.Width,
				Height:  videoStream.Height,
				FPS:     ratio(videoStream.AvgFrameRate),
				PixFmt:  videoStream.PixFmt,
				Bitrate: num(videoStream.BitRate),
				Color:   cs,
			},
		}
	}
	if audioStream != nil {
		if meta.Info == nil {
			meta.Info = &MediaInfo{}
		}
		meta.Info.Audio = &MediaAudioInfo{
			Codec:      audioStream.CodecName,
			Layout:     audioStream.ChannelLayout,
			Channels:   audioStream.Channels,
			SampleRate: num(audioStream.SampleRate),
			Bitrate:    num(audioStream.BitRate),
		}
	}
	if meta.Info != nil && meta.Kind != KindImage {
		meta.Info.Duration, _ = strconv.ParseFloat(parsed.Format.Duration, 64)
	}

	// Images carry no duration in ffprobe's format section (a still frame
	// has none); only time-based media must have one. Parsing AFTER the
	// kind switch is what makes image imports possible at all - the old
	// code parsed up front and rejected every image with a ParseFloat("")
	// error.
	if meta.Kind != KindImage {
		duration, err := strconv.ParseFloat(parsed.Format.Duration, 64)
		if err != nil {
			return Metadata{}, fmt.Errorf("media: could not determine duration for %q: %w", path, err)
		}
		meta.Duration = duration
	}

	return meta, nil
}

// MeasureLoudness measures the whole first audio stream using EBU R128.
// Store a static gain towards -23 LUFS, limited to -1 dBTP headroom.
func MeasureLoudness(path string) (float64, error) {
	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostats", "-nostdin", "-i", path,
		"-map", "0:a:0", "-af", "loudnorm=I=-23:TP=-1:LRA=7:print_format=json", "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("media: loudness measurement: %w", err)
	}
	start, end := bytes.LastIndexByte(out, '{'), bytes.LastIndexByte(out, '}')
	if start < 0 || end < start {
		return 0, fmt.Errorf("media: missing loudness report")
	}
	var report struct {
		Integrated string `json:"input_i"`
		TruePeak   string `json:"input_tp"`
	}
	if err := json.Unmarshal(out[start:end+1], &report); err != nil {
		return 0, err
	}
	i, ierr := strconv.ParseFloat(report.Integrated, 64)
	tp, terr := strconv.ParseFloat(report.TruePeak, 64)
	if ierr != nil || terr != nil || math.IsNaN(i) || math.IsInf(i, 0) || math.IsNaN(tp) || math.IsInf(tp, 0) {
		return 0, fmt.Errorf("media: no measurable loudness (silent or too short)")
	}
	return math.Min(-23-i, -1-tp), nil
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
		// Wave fills the 640px width; silent stretches would otherwise be
		// indistinguishable from background, so a thin centre line runs the
		// full width under the envelope.
		cmd = exec.Command("ffmpeg", "-y", "-i", srcPath, "-filter_complex",
			"showwavespic=s=640x360:colors=#a855f7,drawbox=y=(ih/2)-1:w=iw:h=2:color=#6b2fa0:t=fill,format=yuvj420p", "-frames:v", "1", outPath)
	default:
		return fmt.Errorf("media: unknown kind %q for %q", kind, srcPath)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("media: ffmpeg thumbnail generation failed for %q: %w: %s", srcPath, err, out)
	}
	return nil
}

// GeneratePeaks decodes srcPath's audio to a per-duration amplitude envelope
// (values 0..1, one entry per bucket) suitable for rendering a trim timeline
// waveform. It downmixes to mono float32 and reads it back, so images (no
// audio) yield an all-zero envelope - callers may treat that as "no audio".
func GeneratePeaks(srcPath string) ([]float64, error) {
	// One bucket per ~10ms of audio (the analysis stream is downsampled to
	// 100 Hz, so bins == samples for short files), so zooming the trim
	// timeline reveals progressively finer detail instead of spreading a
	// fixed coarse envelope. Long files are capped to keep the stored (and
	// per-inspector-render) JSON payload small.
	const peakBucketsCap = MaxWaveformBins
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
	peakBuckets := sampleCount
	if peakBuckets < MinWaveformBins {
		peakBuckets = MinWaveformBins
	}
	if peakBuckets > peakBucketsCap {
		peakBuckets = peakBucketsCap
	}
	return samplesToPeaks(buf.Bytes(), peakBuckets), nil
}

// GeneratePeaksWindow decodes the [from,to) slice of srcPath into exactly
// `bins` amplitude buckets (values 0..1), at a rate high enough that even a
// sub-second zoom window still resolves per-pixel detail. The trim timeline
// asks for this when the stored envelope can no longer fill the canvas, and
// resamples the result to the visible pixel count so bar density stays
// constant at every zoom depth.
func GeneratePeaksWindow(srcPath string, from, to float64, bins int) ([]float64, error) {
	if from < 0 || to-from < 0.01 {
		return nil, fmt.Errorf("media: invalid window [%v, %v)", from, to)
	}
	if bins < 16 || bins > MaxWaveformBins {
		return nil, fmt.Errorf("media: bin count %d out of range [16, %d]", bins, MaxWaveformBins)
	}
	// Sample rate scales with zoom depth: a shallow window needs far fewer
	// samples than a deep one to fill the bucket count, so this never
	// decodes a whole long file at deep-zoom resolution. 4x the bucket count
	// (clamped 20..1000 Hz) keeps every bucket fed while capping the stream
	// at ~16 bytes per bin.
	rate := int(math.Ceil(4 * float64(bins) / (to - from)))
	if rate < 20 {
		rate = 20
	}
	if rate > 1000 {
		rate = 1000
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-ss", fmt.Sprintf("%.3f", from),
		"-i", srcPath, "-t", fmt.Sprintf("%.3f", to-from),
		"-vn", "-ac", "1", "-ar", strconv.Itoa(rate), "-f", "f32le", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("media: opening sample stream for %q: %w", srcPath, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("media: starting window analysis for %q: %w", srcPath, err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, out); err != nil {
		return nil, fmt.Errorf("media: reading window samples for %q: %w", srcPath, err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("media: window analysis failed for %q: %w", srcPath, err)
	}
	return samplesToPeaks(buf.Bytes(), bins), nil
}

// samplesToPeaks max-holds little-endian float32 samples into peakBuckets
// evenly-spaced amplitude buckets, normalized by the loudest sample.
func samplesToPeaks(raw []byte, peakBuckets int) []float64 {
	peaks := make([]float64, peakBuckets)
	sampleCount := len(raw) / 4
	if sampleCount == 0 {
		return peaks
	}
	per := (sampleCount + peakBuckets - 1) / peakBuckets
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
	return peaks
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
