package av1dec

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-gst/go-gst/gst"
)

// A short AV1 clip decodes through decodebin, which must pick cutepidav1ddec.
func TestDecodebinPicksDav1d(t *testing.T) {
	gst.Init(nil)
	if !Register() {
		t.Skip("libdav1d.so.7 not installed")
	}
	if exec.Command("ffmpeg", "-hide_banner", "-h", "encoder=libsvtav1").Run() != nil {
		t.Skip("no AV1 encoder to make a test clip")
	}
	clip := filepath.Join(t.TempDir(), "t.mkv")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=s=320x240:r=30:d=1",
		"-c:v", "libsvtav1", "-preset", "12", "-pix_fmt", "yuv420p", clip).CombinedOutput(); err != nil {
		t.Skipf("cannot make the AV1 clip: %v %s", err, out)
	}
	p, err := gst.NewPipelineFromString("filesrc location=" + clip + " ! decodebin name=d ! fakesink name=s")
	if err != nil {
		t.Fatal(err)
	}
	defer p.SetState(gst.StateNull)
	p.SetState(gst.StatePlaying)
	msg := p.GetPipelineBus().TimedPopFiltered(gst.ClockTime(20e9), gst.MessageEOS|gst.MessageError)
	if msg == nil || msg.Type() != gst.MessageEOS {
		t.Fatalf("no EOS: %v", msg)
	}
	d, _ := p.GetElementByName("d")
	found := false
	els, err := gst.ToGstBin(d).GetElementsRecursive()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range els {
		if f := e.GetFactory(); f != nil && f.GetName() == "cutepidav1ddec" {
			found = true
		}
	}
	if !found {
		t.Error("decodebin did not use cutepidav1ddec")
	}
	_ = os.Remove(clip)
}
