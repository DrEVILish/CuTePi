package gsp

import (
	"CuTePi/gsp/glwall"
	"CuTePi/gsp/planewall"
	"CuTePi/gsp/scanout"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/go-gst/go-gst/gst"

	"CuTePi/config"
	"CuTePi/logs"
)

type pipelineSpec struct {
	isTest      bool
	testPattern string
	filename    string
	endpointURL string
	liveTitle   string
	// warmSink routes the video branch to fakesink instead of the wall: a
	// warm-slot build may not display anything or its first frame would
	// colour over the live cue (see Warm). Activation relinks the wall.
	warmSink bool
	// liveStandby: a live page built ahead of its GO (livestandby.go); its
	// renderer runs until the pipeline plays or is retired.
	liveStandby bool
	opts        LoadOpts // the cue's playback options (geometry, rotation, …)
	overlay     bool     // test pattern: print the display mode on it
}

// rotationMethod maps cue rotation degrees to a videoflip method.
func rotationMethod(deg int) string {
	switch deg {
	case 90:
		return "clockwise"
	case 180:
		return "rotate-180"
	case 270:
		return "counterclockwise"
	default:
		return "none"
	}
}

// flipMethod maps cue mirror mode to a videoflip method.
func flipMethod(f string) string {
	switch f {
	case "h":
		return "horizontal-flip"
	case "v":
		return "vertical-flip"
	default:
		return "none"
	}
}

// buildPipeline constructs either a video-test-pattern pipeline or a
// file-playback pipeline (filesrc -> decodebin -> auto{audio,video}sink),
// depending on spec. This replaces the previous buildFilePipeline/
// buildTestPipeline, which were ~90% duplicated.
// endpointFPS is the live-page render rate when its frames are copied into
// the display plane (CPU path, or GL with a readback): TimerPi pages are text
// and countdowns (changing once a second; the overtime pulse twice), and
// fades run on the display plane, not the page, so 15 fps loses nothing
// visible there. Measured on a Pi 4 at 1080p: each copied frame costs CuTePi
// about 54% of a core at 30 fps, and a GL readback caps an animated page at
// 33-50 fps. The zero-copy path renders at the display's refresh rate.
const endpointFPS = 15

// livePageChain is what follows wpevideosrc. With GL (the default), WPE renders
// and composites the page on the GPU. On the KMS plane wall, wpedmabuf
// (gsp/scanout) GPU-copies each frame into a linear buffer the plane scans
// out, so the page runs at the display rate with no CPU copy: measured on a
// Pi 4 at 1080p60, an animated page (TimerPi's blue-future background) at 60.0
// fps, against 33-35 fps through gldownload, which reads every frame back
// into system memory and is still the path elsewhere (other wall sinks,
// 90/270° software rotation). Without GL memory WPE falls back to painting and
// compositing the whole page on the CPU: the same page cost WebKit 340% of a
// core that way against 198% with GL, for identical frames (h264-pi4 livepage
// results). CUTEPI_LIVEPAGE_GL=0 forces the CPU path.
func livePageChain(dw, dh, hz int, zeroCopy bool) ([]*gst.Element, error) {
	raw := func() ([]*gst.Element, error) {
		caps, err := gst.NewElement("capsfilter")
		if err != nil {
			return nil, err
		}
		caps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw,format=BGRA,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", dw, dh, endpointFPS)))
		return []*gst.Element{caps}, nil
	}
	if os.Getenv("CUTEPI_LIVEPAGE_GL") == "0" {
		return raw()
	}
	glCaps, err := gst.NewElement("capsfilter")
	if err != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: live page: no GStreamer GL (gstreamer1.0-gl), rendering on the CPU")
		return raw()
	}
	if zeroCopy && scanout.Register() {
		if out, err := gst.NewElement("wpedmabuf"); err == nil {
			if hz <= 0 {
				hz = 60
			}
			glCaps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw(memory:GLMemory),format=RGBA,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", dw, dh, hz)))
			return []*gst.Element{glCaps, out}, nil
		}
	}
	down, err2 := gst.NewElement("gldownload")
	outCaps, err3 := gst.NewElement("capsfilter")
	if err2 != nil || err3 != nil {
		logs.Printf(logs.GSPPipeDebug, "gsp: live page: no GStreamer GL (gstreamer1.0-gl), rendering on the CPU")
		return raw()
	}
	glCaps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw(memory:GLMemory),format=RGBA,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", dw, dh, endpointFPS)))
	outCaps.Set("caps", gst.NewCapsFromString("video/x-raw,format=RGBA"))
	return []*gst.Element{glCaps, down, outCaps}, nil
}

func buildPipeline(spec pipelineSpec) (*gst.Pipeline, error) {
	NoteBusy() // a cue starting: background WebKit preloads stop now (quiet.go)
	pipeline, err := gst.NewPipeline("")
	if err != nil {
		return nil, err
	}
	trackAlive(pipeline)
	onDeepElementAdded(pipeline.Element, deepNoQoS)
	if glOpen {
		glwall.UseSystemClock(pipeline) // the wall's clock: the bridge maps running times onto it
	}
	glwall.ShareGLDisplay(pipeline) // live pages' GL (glwall.c: one display for the process)

	var src *gst.Element
	var srcChain []*gst.Element // test patterns: sized to the display, optional mode label
	if spec.isTest {
		src, err = gst.NewElement("videotestsrc")
		if err != nil {
			return nil, err
		}
		// Enum property: SetArg parses the nick ("red", "checkers-1"); Set with
		// a Go string is a type mismatch GObject ignores (every pattern then
		// stayed the default SMPTE bars).
		src.SetArg("pattern", spec.testPattern)
		dw, dh, hz := DisplayMode()
		if dw <= 0 || dh <= 0 {
			dw, dh = 1920, 1080
		}
		rate := testPatternRate(spec.testPattern, hz)
		caps, err := gst.NewElement("capsfilter")
		if err != nil {
			return nil, err
		}
		gw, gh := dw, dh
		if spec.testPattern == "snow" {
			// Random noise needs no native resolution; the display layer
			// scales it up for free (1080p noise at 30 fps pins a core).
			gw, gh = dw/3&^1, dh/3&^1
		}
		caps.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw,format=I420,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", gw, gh, rate)))
		srcChain = append(srcChain, caps)
		if spec.overlay {
			txt, err := gst.NewElement("textoverlay")
			if err != nil {
				return nil, err
			}
			label := fmt.Sprintf("%d × %d", dw, dh)
			if hz > 0 {
				label += fmt.Sprintf(" @ %d Hz", hz)
			}
			txt.Set("text", label)
			txt.Set("font-desc", "Sans Bold 40")
			txt.SetArg("valignment", "top")
			txt.SetArg("halignment", "left")
			txt.Set("shaded-background", true)
			// Plain raw video after the overlay: textoverlay then blends the
			// text itself instead of offering an overlay-composition caps
			// feature that the decodebin -> wall chain cannot link.
			plain, err := gst.NewElement("capsfilter")
			if err != nil {
				return nil, err
			}
			plain.Set("caps", gst.NewCapsFromString("video/x-raw"))
			srcChain = append(srcChain, txt, plain)
		}
	} else if spec.endpointURL != "" {
		// Live page (DESIGN §12.14): WPE renders the page off-screen (no
		// window, no DRM master) at the display's size. Its raw frames pass
		// straight through decodebin into the same wall tail as any video.
		dw, dh, hz := DisplayMode()
		if dw <= 0 || dh <= 0 {
			dw, dh = 1920, 1080
		}
		if hz <= 0 {
			hz = 60
		}
		// Zero-copy where the frames need no CPU turn: on the KMS wall the
		// plane scans wpedmabuf's buffers out; on the GPU wall they take the
		// DMABuf route into the mixer (glupload imports them).
		_, hwRotate := rotationBits(spec.opts.Rotation, spec.opts.Flip)
		zeroCopy := kmsWall() != nil && hwRotate && !spec.warmSink
		if !spec.warmSink && liveRemote(spec.opts) {
			// WebKit in a child process (gsp/scanout/remote.c): a view torn
			// down in the service could crash it.
			src, err = gst.NewElementWithName("appsrc", liveSrcName)
			if err != nil {
				return nil, err
			}
			src.Set("caps", gst.NewCapsFromString(fmt.Sprintf("video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format=AB24,width=%d,height=%d,framerate=%d/1,pixel-aspect-ratio=1/1", dw, dh, hz)))
			src.SetArg("format", "time") // enum: Set(gst.FormatTime) was silently ignored (segments stayed in bytes: a GStreamer assertion per frame)
			src.Set("is-live", true)
			src.Set("do-timestamp", true)
			src.Set("max-buffers", uint64(2))
			src.SetArg("leaky-type", "downstream") // an old frame goes back to the renderer, never blocks it
			if err := scanout.StartRemote(src, endpointOpenURL(spec.endpointURL), dw, dh, hz, false, spec.liveStandby); err != nil {
				return nil, fmt.Errorf("live page renderer: %w", err)
			}
		} else {
			src, err = gst.NewElement("wpevideosrc")
			if err != nil {
				return nil, errors.New("live pages need the WPE renderer: install the gstreamer1.0-wpe package")
			}
			src.Set("location", endpointOpenURL(spec.endpointURL))
			chain, err := livePageChain(dw, dh, hz, zeroCopy)
			if err != nil {
				return nil, err
			}
			srcChain = append(srcChain, chain...)
		}
	} else {
		src, err = gst.NewElement("filesrc")
		if err != nil {
			return nil, err
		}
		srcFile := config.MediaLocation() + "/" + spec.filename
		if filepath.IsAbs(spec.filename) {
			srcFile = spec.filename // generated test signals live outside the pool
		} else if SourceLookup != nil {
			if p := SourceLookup(spec.filename); p != "" {
				srcFile = p
			}
		}
		src.Set("location", srcFile)
	}

	// GPU wall: decodebin3 plugs the decoder against the real downstream, so a
	// stateless V4L2 decoder negotiates DMABuf output; decodebin exposes such a
	// pad with system-memory tiled caps first and never renegotiates
	// (TEST_REPORT, GPU wall step 2).
	// Live pages stay on decodebin: its raw passthrough is what was verified
	// for wpevideosrc's frames.
	// Plane wall presenter: HEVC 4:2:0, 8-bit (NV12) and 10-bit (P030), goes
	// to the plane as the decoder's tiled DMABuf frames (cutepiplanesink
	// gathers their UV, §6.1.4), which takes decodebin3 as on the GPU wall.
	// Only these files: decodebin3 exposes software decoders' pads before
	// their size is known (the plane wall's crop needs it). 10-bit HEVC on
	// the decoder's untiled system-memory copy played at 0-1.4 fps.
	planeHEVC := false
	if planewall.IsOpen() && spec.endpointURL == "" && !spec.isTest && CodecLookup != nil {
		c := CodecLookup(spec.filename)
		planeHEVC = c == "hevc/yuv420p" || c == "hevc/yuv420p10le"
	}
	decoderBin := "decodebin"
	if (glOpen && spec.endpointURL == "") || planeHEVC {
		decoderBin = "decodebin3"
	}
	decodebin, err := gst.NewElement(decoderBin)
	if err != nil {
		return nil, err
	}

	head := append(append([]*gst.Element{src}, srcChain...), decodebin)
	if err := pipeline.AddMany(head...); err != nil {
		return nil, fmt.Errorf("building the source chain: %w", err)
	}
	if err := gst.ElementLinkMany(head...); err != nil {
		return nil, fmt.Errorf("linking the source chain: %w", err)
	}

	// Connect to decodebin's pad-added signal, emitted whenever it finds a
	// stream from the input and a way to decode it to raw format. decodebin
	// adds a src-pad for the raw stream, which we link into a playback tail.
	var primaryMu sync.Mutex
	primary := map[string]string{}
	onPadAdded(pipeline, decodebin, func(self *gst.Element, srcPad *gst.Pad) {
		var isAudio, isVideo bool
		caps := srcPad.GetCurrentCaps()
		if caps == nil {
			// decodebin3 exposes a pad before it carries caps: ask what it
			// can produce (the decoder's template; the format is not fixed yet).
			caps = srcPad.QueryCaps(nil)
		}
		if caps == nil {
			caps = gst.NewCapsFromString("video/x-raw")
			if strings.HasPrefix(srcPad.GetName(), "audio") {
				caps = gst.NewCapsFromString("audio/x-raw")
			}
		}
		for i := 0; i < caps.GetSize(); i++ {
			st := caps.GetStructureAt(i)
			if strings.HasPrefix(st.Name(), "audio/") {
				isAudio = true
			}
			if strings.HasPrefix(st.Name(), "video/") {
				isVideo = true
			}
		}

		logs.Printf(logs.GSPPadAdded, "gsp: new pad added, is_audio=%v, is_video=%v\n", isAudio, isVideo)

		if !isAudio && !isVideo {
			err := errors.New("could not detect media stream type")
			msg := gst.NewErrorMessage(self, gst.NewGError(1, err), "", nil)
			pipeline.GetPipelineBus().Post(msg)
			return
		}

		// Only the first audio and the first video stream get a real output
		// chain. A file with several audio tracks (dual-language mp4, mp3 plus
		// AC3) would otherwise build a second sink per stream: two sinks on one
		// device fail to preroll and the whole cue never starts. Extra streams
		// are drained into a fakesink so decodebin still sees every pad linked.
		kind := "video"
		if isAudio {
			kind = "audio"
		}
		streamID := srcPad.GetStreamID()
		primaryMu.Lock()
		if primary[kind] == "" {
			primary[kind] = streamID
		}
		isPrimary := primary[kind] == streamID
		primaryMu.Unlock()
		if !isPrimary {
			linkExtraStream(pipeline, srcPad, kind, streamID)
			return
		}

		var elementNames []string
		queueName := "video-queue"
		if isAudio {
			queueName = "audio-queue"
			// Audio routing: every cue's sound goes into the audio bus
			// (audiobus.go: one mixer on the configured device, so running
			// cues are heard together, §6.1.2).
			sink := "autoaudiosink"
			if !spec.isTest && !spec.warmSink {
				sink = "interaudiosink"
			}
			if spec.warmSink {
				// Warm-slot audio prerolls into a fakesink: the real device
				// is only opened at activation (warmWireAudio), so the slot
				// can never hold a hw device the live cue owns, and the cue
				// lands on the configured device/rate/channels like a cold
				// load does.
				sink = "fakesink"
			}
			elementNames = []string{"queue", "audioconvert", "audioresample", "volume", "audiopanorama", "scaletempo", sink}
		} else if glOpen && !spec.warmSink {
			// GPU wall: the cue feeds the mixer through an appsink (gllayer.go).
			dmabufTail := glUseDMABuf(srcPad, caps, spec.opts, spec.isTest, spec.filename)
			elementNames = glVideoTail(dmabufTail, glTailFormat(caps, spec.filename),
				glDirection(spec.opts.Rotation, spec.opts.Flip) != dirIdentity, !spec.isTest && glISPCanTake(srcPad))
			elementNames = glDeinterlace(elementNames, !dmabufTail && glInterlaced(caps, spec.filename))
		} else if kmsWall() != nil {
			// KMS wall: own display plane, hardware scaling/blending.
			// 8-bit HEVC on the presenter: DMABuf even when its pad has not
			// fixed it yet (the decoder's untiled system-memory output is
			// ~1 fps); cutepiplanesink takes its tiled frames (§6.1.4).
			dmabuf := dmaBufUpstream(srcPad)
			if planeHEVC {
				dmabuf = glDmaBufCapable(srcPad)
			}
			// Interlaced video on the presenter: the software tail with a
			// bob deinterlacer (pwDeinterlace), decoder frames included.
			interlaced := planewall.IsOpen() && !spec.isTest && !planeHEVC && glInterlaced(caps, spec.filename)
			fromDMABuf := interlaced && dmabuf
			if interlaced {
				dmabuf = false
			}
			elementNames = kmsVideoTail(!spec.isTest && dmabuf, spec.opts)
			if interlaced {
				elementNames = pwDeinterlace(elementNames, fromDMABuf)
			}
		} else {
			// Two videoflip stages (rotate, then mirror) so a cue can
			// combine e.g. 90° with a horizontal mirror; method=none
			// passes frames through untouched.
			// The second videoconvert sits after the flips: videoflip handles
			// only a subset of raw formats, so without a converter on the sink
			// side a picky wall sink (fbdevsink's RGB16 framebuffer) cannot
			// negotiate at all — decodebin then fails delayed linking and the
			// cue silently never plays. Passthrough cost when formats match.
			stem := videoStem(!spec.isTest && dmaBufUpstream(srcPad))
			elementNames = append(stem, wallVideoSink())
			if d := config.Display(); d.Resolution != "" && !d.UseEDID && gstCapsWidthHeightOK(d.Resolution) {
				// Manual wall resolution: retune the decoded stream to the
				// destination size (videoscale picks it up from the caps).
				elementNames = append(videoStem(!spec.isTest && dmaBufUpstream(srcPad)), "capsfilter", wallVideoSink())
			}
			if spec.warmSink {
				// Warm-slot video preroll: decode into fakesink — silent, no
				// wall takeover. warm-vf-tail is the relink anchor at activation.
				// Warm slots are always file decodes; the download applies
				// exactly when the pad carries DMABuf (see dmaBufUpstream).
				elementNames = append(videoStem(dmaBufUpstream(srcPad)), "fakesink")
			}
		}
		queueName += "-" + srcPad.GetStreamID()
		// decodebin recreates its pads after Stop -> Play. Reuse the tail;
		// an abandoned, unlinked sink would otherwise prevent preroll forever.
		if queue, err := pipeline.GetElementByName(queueName); err == nil && queue != nil {
			if ret := srcPad.Link(queue.GetStaticPad("sink")); ret != gst.PadLinkOK {
				failLink(pipeline, self, fmt.Errorf("relinking the %s stream: %v", kind, ret))
			}
			return
		}

		elements, err := gst.NewElementMany(elementNames...)
		if err != nil {
			msg := gst.NewErrorMessage(self, gst.NewGError(2, err), "could not create playback elements", nil)
			pipeline.GetPipelineBus().Post(msg)
			return
		}
		elements[0].Set("name", queueName)
		// Role map built from the same slice the elements were created
		// from, so it stays correct whatever optional stages the chain
		// carries. (Factory introspection is not reliably exposed, and
		// positional wiring silently mis-aimed every per-cue setting on
		// hardware-decoded pipelines.)
		byFactory := make(map[string][]*gst.Element, len(elements))
		for i, e := range elements {
			f := elementNames[i]
			byFactory[f] = append(byFactory[f], e)
		}
		// Settings-tab output tuning (only real pipelines; the warm slot
		// prerolls with default plumbing so it can never grab a hw device).
		if !spec.warmSink && !spec.isTest {
			if isAudio {
				// The bus carries the device, format and the GPU wall's
				// display delay (audiobus.go).
				elements[len(elements)-1].Set("name", "cue-audio-sink") // found again on resume
				if err := audioBusAttach(pipeline, elements[len(elements)-1]); err != nil {
					failLink(pipeline, self, fmt.Errorf("the audio output: %w", err))
					return
				}
			} else if i := indexOfName(elementNames, "capsfilter"); i >= 0 {
				setResolutionCaps(elements[i])
			}
		}
		if isVideo && glOpen && !spec.warmSink {
			if err := configureGLTail(pipeline, byFactory, elementNames, spec.opts, glUseDMABuf(srcPad, caps, spec.opts, spec.isTest, spec.filename), glTailFormat(caps, spec.filename)); err != nil {
				msg := gst.NewErrorMessage(self, gst.NewGError(3, err), "no wall layer", nil)
				pipeline.GetPipelineBus().Post(msg)
				return
			}
			configureDeinterlace(byFactory, caps, spec.filename)
		} else if isVideo && kmsWall() != nil {
			fw, fh := capsSize(caps)
			if err := configureKMSTail(pipeline, byFactory, spec.opts, fw, fh, !spec.isTest && isStillFile(spec.filename)); err != nil {
				msg := gst.NewErrorMessage(self, gst.NewGError(3, err), "no display layer", nil)
				pipeline.GetPipelineBus().Post(msg)
				return
			}
			configureDeinterlace(byFactory, caps, spec.filename)
		}
		if spec.warmSink && isAudio {
			last := len(elements) - 1
			elements[last-1].Set("name", "warm-af-tail")
			elements[last].Set("name", "warm-audio-sink")
			elements[last].Set("sync", false)
		}
		if spec.warmSink && isVideo {
			if i := indexOfName(elementNames, "fakesink"); i > 0 {
				// Relink anchors for install-time: whatever feeds the fake
				// sink is the output edge (post-flip converter, or the
				// capsfilter when a wall resolution is configured); the
				// fake sink gets a findable name (startPlayback's pool).
				elements[i-1].Set("name", "warm-vf-tail")
				elements[i].Set("name", "warm-video-sink")
				elements[i].Set("sync", false)
			}
		}
		if err := pipeline.AddMany(elements...); err != nil {
			failLink(pipeline, self, fmt.Errorf("adding the %s output: %w", kind, err))
			return
		}
		if err := gst.ElementLinkMany(elements...); err != nil {
			failLink(pipeline, self, fmt.Errorf("linking the %s output (%s): %w", kind, strings.Join(elementNames, " ! "), err))
			return
		}

		// Elements must be synced to the pipeline's state, otherwise they
		// stay in Null state and can't process data.
		for _, e := range elements {
			e.SyncStateWithParent()
		}

		// Retain the audio "volume" element so SetVolume can drive it, and
		// apply the per-cue master gain once an audio branch exists. The
		// manager fields are only written if this pipeline is STILL the
		// active one: decodebin's pad-added fires asynchronously, so a rapid
		// load A->B can emit A's pads after B was swapped in - writing A's
		// elements here would make SetVolume/FadeAndStop ramp the dead
		// pipeline while B played unattended.
		if isAudio {
			mgr.mu.Lock()
			if mgr.pipeline == pipeline {
				mgr.volumeEl = elements[3]
				mgr.panEl = elements[4]
				mgr.applyGain()
				elements[4].Set("panorama", float32(mgr.balance))
			}
			mgr.mu.Unlock()
		}
		// Retain the video "videobalance" element so the fade-to-black ramp can
		// drive its brightness; a fresh clip always starts at full brightness.
		// Elements resolve by role from the build slice, never by position:
		// optional stages (the v4l2convert download, the wall-resolution
		// capsfilter) shift indices, and positional wiring silently aimed
		// every setting at the wrong element on hardware-decoded pipelines.
		if isVideo && kmsWall() == nil {
			mgr.mu.Lock()
			if mgr.pipeline == pipeline {
				if el := firstByFactory(byFactory, "videobalance"); el != nil {
					mgr.brightEl = el
					el.Set("brightness", mgr.fadeLevel-1)
				}
				// Per-cue frame geometry (§5.5): letterbox vs stretch on
				// videoscale, rotation then mirror on the two videoflips.
				fit := mgr.fitMode
				if fit != "stretch" {
					fit = "fit"
				}
				if el := firstByFactory(byFactory, "videoscale"); el != nil {
					el.Set("add-borders", fit == "fit")
				}
				flips := byFactory["videoflip"]
				if len(flips) > 0 {
					flips[0].SetArg("method", rotationMethod(mgr.rotation))
				}
				if len(flips) > 1 {
					flips[1].SetArg("method", flipMethod(mgr.flip))
				}
			}
			mgr.mu.Unlock()
		}

		queue := elements[0]
		sinkPad := queue.GetStaticPad("sink")
		if ret := srcPad.Link(sinkPad); ret != gst.PadLinkOK {
			failLink(pipeline, self, fmt.Errorf("linking the decoded %s stream to its output: %v", kind, ret))
			return
		}
		if isVideo && glOpen && !spec.warmSink {
			glRegister(pipeline, byFactory, elementNames, spec.opts, isPackedRGB(glPadFormat(caps, spec.filename)))
		}
	})

	return pipeline, nil
}

// gstCapsWidthHeightOK validates a settings "WxH" resolution for caps use.
func gstCapsWidthHeightOK(res string) bool {
	parts := strings.SplitN(res, "x", 2)
	if len(parts) != 2 {
		return false
	}
	w, e1 := strconv.Atoi(parts[0])
	h, e2 := strconv.Atoi(parts[1])
	return e1 == nil && e2 == nil && w > 0 && h > 0
}

// setResolutionCaps retunes a capsfilter element to the configured wall
// resolution (Display tab). Silent on invalid config: a broken setting must
// never stop the show, only fall back to the media's own size.
func setResolutionCaps(capsEl *gst.Element) {
	d := config.Display()
	if !gstCapsWidthHeightOK(d.Resolution) {
		return
	}
	w, _ := strconv.Atoi(strings.SplitN(d.Resolution, "x", 2)[0])
	h, _ := strconv.Atoi(strings.SplitN(d.Resolution, "x", 2)[1])
	capsEl.Set("caps", gst.NewCapsFromString(
		fmt.Sprintf("video/x-raw,width=%d,height=%d", w, h)))
}
