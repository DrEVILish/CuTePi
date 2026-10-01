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
	gstInit()
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	p, err := gst.NewElement("playbin")
	if err != nil {
		return fmt.Errorf("gsp: decode check: %w", err)
	}
	for prop, factory := range map[string]string{"video-sink": "fakevideosink", "audio-sink": "fakesink", "text-sink": "fakesink"} {
		sink, err := gst.NewElement(factory)
		if err != nil {
			return fmt.Errorf("gsp: decode check: %w", err)
		}
		sink.Set("sync", false)
		p.Set(prop, sink)
	}
	p.Set("uri", (&url.URL{Scheme: "file", Path: abs}).String())
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
				return fmt.Errorf("this system has no GStreamer %s", strings.Join(missing, ", "))
			}
			return nil
		case gst.MessageElement:
			if st := msg.GetStructure(); st != nil && st.Name() == "missing-plugin" {
				missing = append(missing, missingPluginText(st))
			}
		case gst.MessageError:
			if len(missing) > 0 {
				return fmt.Errorf("this system has no GStreamer %s", strings.Join(missing, ", "))
			}
			return fmt.Errorf("GStreamer cannot decode it: %s", msg.ParseError().Error())
		}
	}
	return fmt.Errorf("GStreamer did not finish decoding the first frame within %v", decodeCheckTimeout)
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
