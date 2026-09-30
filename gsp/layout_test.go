package gsp

import "testing"

func TestComputeLayout(t *testing.T) {
	cases := []struct {
		name string
		opts LoadOpts
		sw   bool
		want frameLayout
	}{
		{"fit keeps everything", LoadOpts{FitMode: "fit"}, false, frameLayout{outW: 1920, outH: 1080}},
		{"fill-width crops top/bottom", LoadOpts{FitMode: "fill-width"}, false, frameLayout{cropT: 60, cropB: 60, outW: 1920, outH: 960}},
		{"fill-height on 4:3 box crops sides", LoadOpts{FitMode: "fill-height"}, false, frameLayout{}},
		{"user crop 10% left and 100px top", LoadOpts{CropL: "10%", CropT: "100"}, false, frameLayout{cropL: 192, cropT: 100, outW: 1728, outH: 980}},
		{"fill covers a square box", LoadOpts{FitMode: "fill"}, false, frameLayout{}},
	}
	// 1920x1080 source into a 1920x960 box (fill-width) or a 1920x1080 box.
	for _, c := range cases {
		bw, bh := 1920, 1080
		switch c.name {
		case "fill-width crops top/bottom":
			bh = 960
		case "fill-height on 4:3 box crops sides":
			bw, bh = 1440, 1080
			c.want = frameLayout{cropL: 240, cropR: 240, outW: 1440, outH: 1080}
		case "fill covers a square box":
			bw, bh = 1000, 1000
			c.want = frameLayout{cropL: 420, cropR: 420, outW: 1080, outH: 1080}
		}
		got := computeLayout(c.opts, 1920, 1080, bw, bh, c.sw)
		if got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	// 90° in software: the frame is pre-scaled so the turned picture fits a
	// 1920x1080 box: 1080 tall, 607 wide as shown.
	got := computeLayout(LoadOpts{Rotation: 90}, 1920, 1080, 1920, 1080, true)
	if got.preW != 1080 || got.preH != 606 || got.outH != 1080 || got.outW != 607 {
		t.Errorf("90° pre-scale: %+v", got)
	}
	// Absurd crops are ignored rather than leaving no picture.
	if l := computeLayout(LoadOpts{CropL: "60%", CropR: "60%"}, 1920, 1080, 1920, 1080, false); l.cropL != 0 || l.cropR != 0 {
		t.Errorf("over-crop not ignored: %+v", l)
	}
}
