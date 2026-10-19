package routes

import (
	"context"
	"encoding/json"
	"strings"

	"CuTePi/gsp"
	"CuTePi/logs"
	"CuTePi/media"
)

// ytFormat is one stream of yt-dlp's format list (-J).
type ytFormat struct {
	ID     string  `json:"format_id"`
	VCodec string  `json:"vcodec"`
	Height int     `json:"height"`
	FPS    float64 `json:"fps"`
	TBR    float64 `json:"tbr"`
}

// vcodecName maps yt-dlp's codec strings (avc1.64002A, vp09.00.41.08,
// av01.0.09M.08, hev1...) to ffprobe's names, as plays60 takes them.
func vcodecName(v string) string {
	switch {
	case strings.HasPrefix(v, "avc"), v == "h264":
		return "h264"
	case strings.HasPrefix(v, "hev"), strings.HasPrefix(v, "hvc"), v == "h265", v == "hevc":
		return "hevc"
	case strings.HasPrefix(v, "vp09"), v == "vp9":
		return "vp9"
	case strings.HasPrefix(v, "vp8"):
		return "vp8"
	case strings.HasPrefix(v, "av01"), v == "av1":
		return "av1"
	}
	return v
}

// chooseFormat picks, among the video streams at the best height (at most
// maxHeight) and frame rate on offer, the highest-bitrate one whose codec
// plays at 60 fps here; "" when none does (the import is then refused). A
// lower resolution in a playable codec is never preferred over the best
// picture: output quality first.
func chooseFormat(formats []ytFormat, maxHeight int, gpuWall bool) string {
	id, _ := chooseFormatOffered(formats, maxHeight, gpuWall)
	return id
}

// chooseFormatOffered is chooseFormat plus the codecs offered at the best
// height and frame rate (for the notice when none is supported).
func chooseFormatOffered(formats []ytFormat, maxHeight int, gpuWall bool) (string, []string) {
	bestH, bestFPS := 0, 0.0
	for _, f := range formats {
		if f.VCodec == "" || f.VCodec == "none" || f.Height <= 0 || f.Height > maxHeight {
			continue
		}
		if f.Height > bestH || (f.Height == bestH && f.FPS > bestFPS) {
			bestH, bestFPS = f.Height, f.FPS
		}
	}
	pick, pickTBR := "", -1.0
	var offered []string
	seen := map[string]bool{}
	for _, f := range formats {
		if f.Height != bestH || f.FPS < bestFPS || f.VCodec == "" || f.VCodec == "none" {
			continue
		}
		if name := vcodecName(f.VCodec); !seen[name] {
			seen[name] = true
			offered = append(offered, name)
		}
		if !plays60(vcodecName(f.VCodec), "", gpuWall) {
			continue
		}
		if f.TBR > pickTBR {
			pick, pickTBR = f.ID, f.TBR
		}
	}
	return pick, offered
}

// pickNativeFormat asks yt-dlp for the format list and returns the format
// chooseFormat picks, the codecs offered at the best picture, and whether
// the list was read at all (on any failure yt-dlp makes its own choice and
// the download is checked after it lands).
func pickNativeFormat(ctx context.Context, url string) (string, []string, bool) {
	ctx, cancel := context.WithTimeout(ctx, ytDlpResolveTimeout)
	defer cancel()
	out, err := media.BackgroundCommand(ctx, "yt-dlp", "-J", "--no-playlist", "--no-warnings", "--", url).Output()
	if err != nil {
		return "", nil, false
	}
	var info struct {
		Formats []ytFormat `json:"formats"`
	}
	if json.Unmarshal(out, &info) != nil || len(info.Formats) == 0 {
		return "", nil, false
	}
	id, offered := chooseFormatOffered(info.Formats, 1080, gsp.GPUWall())
	logs.Printf(logs.YDLDownload, "stage=format url=%q native_playable=%q offered=%v (of %d)", url, id, offered, len(info.Formats))
	return id, offered, true
}
