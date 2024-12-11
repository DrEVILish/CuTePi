package gsp

import (

	"github.com/go-gst/go-gst/gst"
)

var (
	pipeline *gst.Pipeline
	bus      *gst.Bus
)

func init() {
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

func Next () {

}

func Prev () {

}

func Stop () {

}

func ShowTest(pattern string) {
  // if pattern not in gst-testpatterns throw error
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
