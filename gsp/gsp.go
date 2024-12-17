package gsp

import (
 	"log"
  "errors"
  "strings"
	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-glib/glib"
)

var (
	pipeline *gst.Pipeline
	bus      *gst.Bus
)

func init() {

  gst.Init(nil)
	// pipeline, err := gst.NewPipeline("")
	// if err != nil {
	// 	fmt.Println("Error initializing pipeline:", err)
	// }
	// bus = pipeline.GetBus()
}

func Play() {
	pipeline.SetState(gst.StatePlaying)
}

func Pause() {
	pipeline.SetState(gst.StatePaused)
}

func TogglePause() {
	// Get the current state of the pipeline
	stateChangeReturn, currentState := pipeline.GetState(gst.StateNull, gst.ClockTimeNone)
	if stateChangeReturn == gst.StateChangeSuccess {
		if currentState == gst.StatePlaying {
			pipeline.SetState(gst.StatePaused)
		} else if currentState == gst.StatePaused {
			pipeline.SetState(gst.StatePlaying)
		}
	}
}

func FadeOut() {
	// fadeOut is not a standard method in go-gst, you might need to implement it yourself
	// or use a different method to achieve the desired effect
}

func Panic() {
	pipeline.SetState(gst.StateNull)
	pipeline = nil
}

func Clear() {
	// clear is not a standard method in go-gst, you might need to implement it yourself
	// or use a different method to achieve the desired effect
}

func GetMimeType(filename string) string {
	// you are calling the same function recursively, this will cause a stack overflow
	// you need to implement the logic to get the mime type of a file

	return filename
}

func GetFileSize(filename string) int64 {
	// you are calling the same function recursively, this will cause a stack overflow
	// you need to implement the logic to get the size of a file

	return 0
}

func GetDuration(filename string) float64 {
	// you are calling the same function recursively, this will cause a stack overflow
	// you need to implement the logic to get the duration of a file

	return 0
}

func Next() {

}

func Prev() {

}

func Stop() {

}

func ShowTest(pattern string) (*gst.Pipeline, error) {

  RunLoop(func(loop *glib.MainLoop) error {
		pipeline, err := buildPipeline()
		if err != nil {
			return err
		}
		return runPipeline(loop, pipeline)
	})

  videotestsrc, err := gst.NewElement("videotestsrc")
  if err != nil {
 	  log.Printf("Error creating videotestsrc: %v", err)
    return nil, err
  }

  glimagesink, err := gst.NewElement("glimagesink")
  if err != nil {
 	  log.Printf("Error creating glimagesink: %v", err)
    return nil, err
  }
  pipeline.AddMany(videotestsrc, glimagesink)
  videotestsrc.Link(glimagesink)

  // Set the pattern to the videotestsrc
  videotestsrc.SetProperty("pattern", pattern)

  videotestsrc.SetState(gst.StatePlaying)

  return pipeline, nil
}

func Load(filename string) {
	// load a file as the next item to play
}

func CurrentPlaying() string {
	return ""
}

func CurrentPosition() float64 {
	return 0
}

func CurrentDuration() float64 {
	return 0
}


func runPipeline(loop *glib.MainLoop, pipeline *gst.Pipeline) error {
	// Start the pipeline
	pipeline.SetState(gst.StatePlaying)

	// Add a message watch to the bus to quit on any error
	pipeline.GetPipelineBus().AddWatch(func(msg *gst.Message) bool {
		var err error

		// If the stream has ended or any element posts an error to the
		// bus, populate error.
		switch msg.Type() {
		case gst.MessageEOS:
			err = errors.New("end-of-stream")
		case gst.MessageError:
			// The parsed error implements the error interface, but also
			// contains additional debug information.
			gerr := msg.ParseError()
			log.Println("go-gst-debug: v%", gerr.DebugString())
			err = gerr
		}

		// If either condition triggered an error, log and quit
		if err != nil {
			log.Println("ERROR: ", err.Error())
			loop.Quit()
			return false
		}

		return true
	})

	// Block on the main loop
	return loop.RunError()
}

func RunLoop(f func(*glib.MainLoop) error) {
	mainLoop := glib.NewMainLoop(glib.MainContextDefault(), false)

	if err := f(mainLoop); err != nil {
		log.Println("ERROR!", err)
	}
}

func buildPipeline() (*gst.Pipeline, error) {
	gst.Init(nil)

	pipeline, err := gst.NewPipeline("")
	if err != nil {
		return nil, err
	}

	src, err := gst.NewElement("filesrc")
	if err != nil {
		return nil, err
	}

	decodebin, err := gst.NewElement("decodebin")
	if err != nil {
		return nil, err
	}

	srcFile := "/home/drevilish/CTP/media/The Quest To Make Unbreakable Glass.mp4"

	src.Set("location", srcFile)

	pipeline.AddMany(src, decodebin)
	src.Link(decodebin)

	// Connect to decodebin's pad-added signal, that is emitted whenever
	// it found another stream from the input file and found a way to decode it to its raw format.
	// decodebin automatically adds a src-pad for this raw stream, which
	// we can use to build the follow-up pipeline.
	decodebin.Connect("pad-added", func(self *gst.Element, srcPad *gst.Pad) {

		// Try to detect whether this is video or audio
		var isAudio, isVideo bool
		caps := srcPad.GetCurrentCaps()
		for i := 0; i < caps.GetSize(); i++ {
			st := caps.GetStructureAt(i)
			if strings.HasPrefix(st.Name(), "audio/") {
				isAudio = true
			}
			if strings.HasPrefix(st.Name(), "video/") {
				isVideo = true
			}
		}

		log.Printf("New pad added, is_audio=%v, is_video=%v\n", isAudio, isVideo)

		if !isAudio && !isVideo {
			err := errors.New("could not detect media stream type")
			// We can send errors directly to the pipeline bus if they occur.
			// These will be handled downstream.
			msg := gst.NewErrorMessage(self, gst.NewGError(1, err), "" , nil)
			pipeline.GetPipelineBus().Post(msg)
			return
		}

		if isAudio {
			// decodebin found a raw audiostream, so we build the follow-up pipeline to
			// play it on the default audio playback device (using autoaudiosink).
			elements, err := gst.NewElementMany("queue", "audioconvert", "audioresample", "autoaudiosink")
			if err != nil {
				// We can create custom errors (with optional structures) and send them to the pipeline bus.
				// The first argument reflects the source of the error, the second is the error itself, followed by a debug string.
				msg := gst.NewErrorMessage(self, gst.NewGError(2, err), "Could not create elements for audio pipeline", nil)
				pipeline.GetPipelineBus().Post(msg)
				return
			}
			pipeline.AddMany(elements...)
			gst.ElementLinkMany(elements...)

			// !!ATTENTION!!:
			// This is quite important and people forget it often. Without making sure that
			// the new elements have the same state as the pipeline, things will fail later.
			// They would still be in Null state and can't process data.
			for _, e := range elements {
				e.SyncStateWithParent()
			}

			// The queue was the first element returned above
			queue := elements[0]
			// Get the queue element's sink pad and link the decodebin's newly created
			// src pad for the audio stream to it.
			sinkPad := queue.GetStaticPad("sink")
			srcPad.Link(sinkPad)

		} else if isVideo {
			// decodebin found a raw videostream, so we build the follow-up pipeline to
			// display it using the autovideosink.
			elements, err := gst.NewElementMany("queue", "videoconvert", "videoscale", "autovideosink")
			if err != nil {
				msg := gst.NewErrorMessage(self, gst.NewGError(2, err), "Could not create elements for video pipeline", nil)
				pipeline.GetPipelineBus().Post(msg)
				return
			}
			pipeline.AddMany(elements...)
			gst.ElementLinkMany(elements...)

			for _, e := range elements {
				e.SyncStateWithParent()
			}

			queue := elements[0]
			// Get the queue element's sink pad and link the decodebin's newly created
			// src pad for the video stream to it.
			sinkPad := queue.GetStaticPad("sink")
			srcPad.Link(sinkPad)
		}
	})
	return pipeline, nil
}