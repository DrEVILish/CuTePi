package sysdmabuf

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
)

func psnr(a, b []byte) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var se float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		se += d * d
	}
	if se == 0 {
		return 99
	}
	return 10 * math.Log10(255*255*float64(len(a))/se)
}

// Every frame the GPU samples from a dma-heap buffer is that frame, not a
// stale one: against the same frame uploaded from system memory it matches
// far better than against the frame before (a moving zone plate: every frame different).
// One pipeline for everything: a second GstGL pipeline after the first is
// finalised finds the EGL display terminated (h264-pi4 FINDINGS §8).
func TestGPUSeesEachFrame(t *testing.T) {
	if _, err := os.Stat("/dev/dri/renderD128"); err != nil {
		t.Skip("no GPU")
	}
	for k, v := range map[string]string{"GST_GL_API": "gles2", "GST_GL_PLATFORM": "egl", "GST_GL_WINDOW": "surfaceless"} {
		t.Setenv(k, v)
	}
	gst.Init(nil)
	if !Register() {
		t.Skip("no system dma-heap")
	}
	const n = 12
	formats := []string{"I420", "NV12"}
	var desc []string
	for _, f := range formats {
		desc = append(desc, fmt.Sprintf("videotestsrc num-buffers=%d pattern=zone-plate kt=20 kx2=20 ky2=20 ! video/x-raw,format=%s,width=1920,height=1080,framerate=60/1 ! tee name=t%s", n, f, f))
		// The heap branch's glupload is linked by hand below: before a GL
		// context exists glupload offers no caps for DMABuf input, so
		// gst_parse's checked link would fail.
		desc = append(desc, fmt.Sprintf("t%s. ! queue ! glupload ! glcolorconvert ! video/x-raw(memory:GLMemory),format=RGBA,texture-target=2D ! gldownload ! video/x-raw,format=RGBA ! appsink name=ref%s sync=false", f, f),
			fmt.Sprintf("t%s. ! queue ! cutepisysdmabuf name=h%s", f, f),
			fmt.Sprintf("glupload name=u%s ! glcolorconvert ! video/x-raw(memory:GLMemory),format=RGBA,texture-target=2D ! gldownload ! video/x-raw,format=RGBA ! appsink name=heap%s sync=false", f, f))
	}
	p, err := gst.NewPipelineFromString(strings.Join(desc, " "))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range formats {
		h, _ := p.GetElementByName("h" + f)
		u, _ := p.GetElementByName("u" + f)
		if r := h.GetStaticPad("src").LinkFull(u.GetStaticPad("sink"), gst.PadLinkCheckNothing); r != gst.PadLinkOK {
			t.Fatalf("%s: link: %v", f, r)
		}
	}
	defer p.SetState(gst.StateNull)
	got := map[string][][]byte{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	p.SetState(gst.StatePlaying) // before pulling: a stopped appsink returns nothing at once
	for _, f := range formats {
		for _, br := range []string{"ref", "heap"} {
			el, _ := p.GetElementByName(br + f)
			sink, key := app.SinkFromElement(el), br+f
			wg.Add(1)
			go func() {
				defer wg.Done()
				for s := sink.PullSample(); s != nil; s = sink.PullSample() {
					buf := s.GetBuffer()
					m := buf.Map(gst.MapRead)
					b := append([]byte(nil), m.Bytes()...)
					buf.Unmap()
					mu.Lock()
					got[key] = append(got[key], b)
					mu.Unlock()
				}
			}()
		}
	}
	if msg := p.GetPipelineBus().TimedPopFiltered(gst.ClockTime(30e9), gst.MessageEOS|gst.MessageError); msg == nil || msg.Type() != gst.MessageEOS {
		t.Fatalf("no EOS: %v", msg)
	}
	wg.Wait()
	for _, f := range formats {
		ref, heap := got["ref"+f], got["heap"+f]
		if len(heap) != n || len(ref) != n {
			t.Fatalf("%s: %d frames through the dma-heap, %d direct", f, len(heap), len(ref))
		}
		for i := range heap {
			same, prev := psnr(heap[i], ref[i]), 0.0
			if i > 0 {
				prev = psnr(heap[i], ref[i-1])
			}
			t.Logf("%s frame %d: %.1f dB against its own reference, %.1f against the one before", f, i, same, prev)
			if same < 30 || same < prev+10 {
				t.Errorf("%s frame %d: not its own picture", f, i)
			}
		}
	}
}
