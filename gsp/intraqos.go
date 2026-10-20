package gsp

import "github.com/go-gst/go-gst/gst"

// intraOnlyDecoders decode every frame on its own (FFmpeg's
// AV_CODEC_PROP_INTRA_ONLY codecs, and CuTePi's and GStreamer's decoders for
// them): a late frame cannot be skipped before it is decoded, so QoS from a
// display sink (the plane wall's video sink, the GPU wall's pump) only throws
// decoded frames away. Run at about real time, such a decoder never caught up
// and dropped nearly every frame for seconds (DNxHR HQ: 1-2 shown a second;
// ProRes Proxy started during a burst of other work: 3 a second).
var intraOnlyDecoders = map[string]bool{
	"avdec_prores": true, "avdec_dnxhd": true, "avdec_hap": true, "avdec_mjpeg": true, "avdec_png": true,
	"avdec_cfhd": true, "avdec_ffv1": true, "avdec_jpeg2000": true, "avdec_utvideo": true, "avdec_magicyuv": true,
	"avdec_huffyuv": true, "avdec_ffvhuff": true, "avdec_dvvideo": true, "avdec_v210": true, "avdec_qoi": true,
	"jpegdec": true, "pngdec": true, "cutepimjpegdec": true, "cutepipngvdec": true, "cutepicfhddec": true,
}

// intraNoQoS turns QoS off on an intra-only decoder as it joins a cue's
// pipeline: late frames are then shown as they come and the wall catches up
// (the GPU wall's pump passes over late frames when a newer one waits).
func intraNoQoS(e *gst.Element) {
	f := e.GetFactory()
	if f == nil || !intraOnlyDecoders[f.GetName()] {
		return
	}
	if _, err := e.GetPropertyType("qos"); err == nil {
		e.Set("qos", false)
	}
}
