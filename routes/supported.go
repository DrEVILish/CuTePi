package routes

import (
	"context"
	"strings"

	"CuTePi/gsp"
	"CuTePi/media"
)

// What CuTePi supports: video that plays at 1080p60 with a smooth fade in
// and out on the display path in use, measured on the Pi 4 (README "Codec
// support"; DESIGN §12.15). URL imports in anything else are refused with
// supportedNotice, never converted.
type supportedCodec struct {
	name         string // ffprobe codec_name
	label        string
	gpu, pw, kms bool // supported on the GPU wall / the plane wall presenter / the kmssink plane wall
	eightBit     bool // 8-bit only (hardware H.264: 10-bit decodes in software, 45-50 fps)
}

// The presenter's column is from the support runs of 2026-10-20 (README, three passes, a majority); the GPU
// wall's and the kmssink wall's from earlier runs.
var supportedCodecs = []supportedCodec{
	{name: "h264", label: "H.264 (8-bit)", gpu: true, pw: true, kms: true, eightBit: true},
	{name: "hevc", label: "HEVC/H.265 (8 and 10-bit)", gpu: true, pw: true},
	{name: "vp8", label: "VP8", gpu: true, pw: true, kms: true},
	{name: "vp9", label: "VP9", pw: true},
	{name: "av1", label: "AV1"},
	{name: "mpeg2video", label: "MPEG-2", gpu: true, pw: true, kms: true},
	{name: "mpeg1video", label: "MPEG-1", gpu: true, pw: true, kms: true},
	{name: "mpeg4", label: "MPEG-4 Part 2", gpu: true, pw: true, kms: true},
	{name: "mjpeg", label: "Motion JPEG", gpu: true, pw: true, kms: true},
	{name: "theora", label: "Theora", pw: true, kms: true},
}

// displayPath is the display path in use, which decides what is supported.
type displayPath int

const (
	pathKMS displayPath = iota
	pathGPU
	pathPresenter
)

func currentPath() displayPath {
	switch {
	case gsp.GPUWall():
		return pathGPU
	case gsp.PresenterWall():
		return pathPresenter
	}
	return pathKMS
}

func (c supportedCodec) on(p displayPath) bool {
	switch p {
	case pathGPU:
		return c.gpu
	case pathPresenter:
		return c.pw
	}
	return c.kms
}

// supportedContainers are the containers the support tests cover.
const supportedContainers = "MP4, MOV, MKV or WebM"

// plays60 reports whether a video stream (ffprobe codec_name and pix_fmt;
// "" when not known) is supported on the display path in use.
func plays60(codec, pixFmt string, path displayPath) bool {
	deep := strings.Contains(pixFmt, "10") || strings.Contains(pixFmt, "12")
	for _, c := range supportedCodecs {
		if c.name == codec {
			return c.on(path) && !(c.eightBit && deep)
		}
	}
	return false
}

// supportedNotice lists the supported codecs and containers for the
// display path in use, for an import that is refused.
func supportedNotice(path displayPath) string {
	var names []string
	for _, c := range supportedCodecs {
		if c.on(path) {
			names = append(names, c.label)
		}
	}
	return "Supported video (1080p60 with smooth fades on this display): " + strings.Join(names, ", ") +
		", in " + supportedContainers + ". Files are not converted on import."
}

// codecLabel names an ffprobe/yt-dlp codec for a person.
func codecLabel(codec string) string {
	for _, c := range supportedCodecs {
		if c.name == codec {
			return c.label
		}
	}
	switch codec {
	case "":
		return "an unknown codec"
	}
	return strings.ToUpper(codec)
}

// probeVideoStream returns the codec name and pixel format of the file's
// first video stream ("" for none).
func probeVideoStream(ctx context.Context, path string) (codec, pixFmt string, err error) {
	out, err := media.BackgroundCommand(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,pix_fmt", "-of", "default=nw=1", "--", path).Output()
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "codec_name":
			codec = v
		case "pix_fmt":
			pixFmt = v
		}
	}
	return codec, pixFmt, err
}
