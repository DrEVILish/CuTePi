package cfhd

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-gst/go-gst/gst"
)

// A CineForm clip decodes through decodebin with cutepicfhddec and every
// frame comes out. The clip is from FFmpeg's encoder, which the SDK partly
// rejects: this also covers the element's FFmpeg fallback.
func TestDecodebinPicksCineForm(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Skip("CineForm library not installed (deploy/build-cineform.sh)")
	}
	clip := filepath.Join(t.TempDir(), "t.mov")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=s=320x240:r=30:d=1",
		"-c:v", "cfhd", "-pix_fmt", "yuv422p10le", clip).CombinedOutput(); err != nil {
		t.Skipf("cannot make the CineForm clip: %v %s", err, out)
	}
	p, err := gst.NewPipelineFromString("filesrc location=" + clip + " ! decodebin name=d ! fakesink name=s")
	if err != nil {
		t.Fatal(err)
	}
	defer p.SetState(gst.StateNull)
	p.SetState(gst.StatePlaying)
	msg := p.GetPipelineBus().TimedPopFiltered(gst.ClockTime(30e9), gst.MessageEOS|gst.MessageError)
	if msg == nil || msg.Type() != gst.MessageEOS {
		t.Fatalf("no EOS: %v", msg)
	}
	d, _ := p.GetElementByName("d")
	els, err := gst.ToGstBin(d).GetElementsRecursive()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range els {
		if f := e.GetFactory(); f != nil && f.GetName() == "cutepicfhddec" {
			found = true
		}
	}
	if !found {
		t.Error("decodebin did not use cutepicfhddec")
	}
	s, _ := p.GetElementByName("s")
	if v, err := s.GetProperty("last-sample"); err != nil || v == nil {
		t.Error("no frame reached the sink")
	}
}
