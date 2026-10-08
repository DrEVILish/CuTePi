package yuvpack

import (
	"testing"

	"github.com/go-gst/go-gst/gst"
)

// 10-bit 4:2:2 in, I420 out; NV12 passes through untouched.
func TestPackNegotiates(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Fatal("cutepiyuvpack did not register")
	}
	for in, want := range map[string]string{"I422_10LE": "I420", "I420_10LE": "I420", "Y42B": "I420", "NV12": "NV12"} {
		p, err := gst.NewPipelineFromString("videotestsrc num-buffers=3 ! video/x-raw,format=" + in +
			",width=320,height=240 ! cutepiyuvpack ! videoconvert ! fakesink name=s")
		if err != nil {
			t.Fatal(err)
		}
		p.SetState(gst.StatePlaying)
		msg := p.GetPipelineBus().TimedPopFiltered(gst.ClockTime(10e9), gst.MessageEOS|gst.MessageError)
		if msg == nil || msg.Type() != gst.MessageEOS {
			t.Fatalf("%s: no EOS: %v", in, msg)
		}
		s, _ := p.GetElementByName("s")
		pad := s.GetStaticPad("sink")
		caps := pad.GetCurrentCaps()
		// videoconvert passes through what the packer produced
		if got, _ := caps.GetStructureAt(0).GetValue("format"); got != want {
			t.Errorf("%s: got %v, want %s", in, got, want)
		}
		p.SetState(gst.StateNull)
	}
}
