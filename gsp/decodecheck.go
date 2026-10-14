package gsp

// Import-time playability check (§2, §5.7): a file is accepted only if the
// playback engine itself can decode it. ffprobe/ffmpeg read far more formats
// than this system's GStreamer may (WMV/WMA without the ASF demuxer, AVIF,
// JPEG XL — codec corpus), and a file that imports but cannot play only
// fails later, at cue time. The check prerolls the file through playbin —
// the same autoplugging and decoder ranks as playback — into fake sinks:
// nothing reaches the display or the audio device.

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-gst/go-gst/gst"
)

// decodeCheckTimeout bounds one check; a large file prerolls well inside it.
const decodeCheckTimeout = 10 * time.Second

// CheckDecodable prerolls path through playbin. nil means GStreamer decodes
// it; otherwise the error says what is missing or broken, in words an
// operator can act on (e.g. a missing demuxer names its media type).
func CheckDecodable(path string) error {
	_, err := DecodeCheck(path)
	return err
}

// DecodeCheck is CheckDecodable that also returns the duration GStreamer
// reports once prerolled (0 when it has none). For animated images it is
// the sum of the frame delays (cutepianimdec), where ffprobe has none for
// APNG, animated WebP, AVIF sequences and JPEG XL animations.
func DecodeCheck(path string) (time.Duration, error) {
	gstInit()
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	p, err := gst.NewElement("playbin")
	if err != nil {
		return 0, fmt.Errorf("gsp: decode check: %w", err)
	}
	for prop, factory := range map[string]string{"video-sink": "fakevideosink", "audio-sink": "fakesink", "text-sink": "fakesink"} {
		sink, err := gst.NewElement(factory)
		if err != nil {
			return 0, fmt.Errorf("gsp: decode check: %w", err)
		}
		sink.Set("sync", false)
		p.Set(prop, sink)
	}
	p.Set("uri", (&url.URL{Scheme: "file", Path: abs}).String())
	p.Connect("deep-element-added", func(_, _, e any) {
		if el, ok := e.(*gst.Element); ok {
			oneDecodeThread(el)
		}
	})
	defer p.SetState(gst.StateNull)
	bus := p.GetBus()
	p.SetState(gst.StatePaused)
	deadline := time.Now().Add(decodeCheckTimeout)
	var missing []string
	for time.Now().Before(deadline) {
		msg := bus.TimedPopFiltered(gst.ClockTime(100*time.Millisecond),
			gst.MessageAsyncDone|gst.MessageError|gst.MessageElement)
		if msg == nil {
			continue
		}
		switch msg.Type() {
		case gst.MessageAsyncDone:
			if len(missing) > 0 {
				// Prerolled, but a stream went without a decoder (e.g. the
				// audio of a file whose video plays).
				return 0, fmt.Errorf("this system has no GStreamer %s", strings.Join(missing, ", "))
			}
			var d time.Duration
			if ok, ns := p.QueryDuration(gst.FormatTime); ok && ns > 0 {
				d = time.Duration(ns)
			}
			return d, nil
		case gst.MessageElement:
			if st := msg.GetStructure(); st != nil && st.Name() == "missing-plugin" {
				missing = append(missing, missingPluginText(st))
			}
		case gst.MessageError:
			if len(missing) > 0 {
				return 0, fmt.Errorf("this system has no GStreamer %s", strings.Join(missing, ", "))
			}
			return 0, fmt.Errorf("GStreamer cannot decode it: %s", msg.ParseError().Error())
		}
	}
	return 0, fmt.Errorf("GStreamer did not finish decoding the first frame within %v", decodeCheckTimeout)
}

// missingPluginText turns a missing-plugin message into "decoder for
// video/x-wmv" / "demuxer for video/x-ms-asf".
func missingPluginText(st *gst.Structure) string {
	kind, _ := st.GetValue("type")
	detail, _ := st.GetValue("detail")
	what := "plugin"
	switch fmt.Sprint(kind) {
	case "decoder":
		what = "decoder"
	case "element":
		what = "element"
	}
	d := fmt.Sprint(detail)
	if c, ok := detail.(*gst.Caps); ok && c != nil {
		d = c.String()
	}
	if i := strings.IndexAny(d, ","); i > 0 {
		d = d[:i] // the media type, without its fields
	}
	if strings.Contains(d, "asf") || strings.Contains(d, "quicktime") || strings.Contains(d, "matroska") {
		if what == "decoder" {
			what = "demuxer"
		}
	}
	return what + " for " + d
}

// oneDecodeThread keeps a decoder in the check to one thread. The check runs
// inside the service at the playback's own priority (its threads are not
// niced like the import's ffmpeg), and a preroll needs only the first frame:
// dav1d started a worker per core here, beside whatever the wall was playing.
func oneDecodeThread(e *gst.Element) {
	f := e.GetFactory()
	if f == nil {
		return
	}
	name := f.GetName()
	switch {
	case name == "cutepidav1ddec":
		e.Set("n-threads", 1)
		e.Set("max-frame-delay", 1)
	case name == "cutepimjpegdec" || name == "cutepipngvdec" || name == "cutepicfhddec":
		e.Set("n-workers", 1)
	case name == "vp8dec" || name == "vp9dec":
		e.Set("threads", uint(1))
	case strings.HasPrefix(name, "avdec_"):
		// Video decoders have max-threads, audio decoders do not. Not by
		// the factory's klass: go-gst's ElementFactory.GetMetadata frees
		// the string GStreamer still owns, corrupting the factory's
		// metadata, after which decodebin no longer plugged FFmpeg's
		// decoders in this process (every ProRes, HAP and AC-3 import was
		// refused).
		if _, err := e.GetPropertyType("max-threads"); err == nil {
			e.Set("max-threads", 1)
		}
	}
}
