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

// Alignment: a fit picture against the box's edges, a fill's overflow off
// the far side, and a small box (no X/Y) against the display's edges.
func TestAlignLayout(t *testing.T) {
	// 4:3 (1440x1080) into a 1920x1080 box: 480 px of side space.
	if l := computeLayout(LoadOpts{Align: "right"}, 1440, 1080, 1920, 1080, false); l.fitX != 480 || l.fitW != 1440 || l.fitH != 1080 {
		t.Errorf("fit right: %+v", l)
	}
	if l := computeLayout(LoadOpts{Align: "left"}, 1440, 1080, 1920, 1080, false); l.fitX != 0 || l.fitW != 1440 {
		t.Errorf("fit left: %+v", l)
	}
	// 1920x1080 into a 1080x1080 box (16:9 letterboxed vertically): 472.5 px, rounded.
	if l := computeLayout(LoadOpts{Align: "bottom"}, 1920, 1080, 1080, 1080, false); l.fitY != 473 || l.fitX != 0 || l.fitH != 608 {
		t.Errorf("fit bottom: %+v", l)
	}
	if l := computeLayout(LoadOpts{}, 1440, 1080, 1920, 1080, false); l.fitW != 0 {
		t.Errorf("unaligned fit must leave centring to the sink: %+v", l)
	}
	// fill-width into 1920x960: 120 px to crop; top keeps the top.
	if l := computeLayout(LoadOpts{FitMode: "fill-width", Align: "top"}, 1920, 1080, 1920, 960, false); l.cropT != 0 || l.cropB != 120 || l.fitW != 0 {
		t.Errorf("fill top: %+v", l)
	}
	if l := computeLayout(LoadOpts{FitMode: "fill-width", Align: "bottom"}, 1920, 1080, 1920, 960, false); l.cropT != 120 || l.cropB != 0 {
		t.Errorf("fill bottom: %+v", l)
	}
	// Turned or mirrored fills: the screen side the alignment keeps maps back
	// to a source edge. 1920x1080 turned to 1080x1920 filling a 1920x1080 box:
	// 1312.5 px of (source-width) overflow.
	for _, c := range []struct {
		opts   LoadOpts
		bw     int
		cl, cr int
		ct, cb int
		desc   string
	}{
		{LoadOpts{FitMode: "fill", Rotation: 90, Align: "top"}, 1920, 0, 1312, 0, 0, "90 top keeps source left"},
		{LoadOpts{FitMode: "fill", Rotation: 90, Align: "bottom"}, 1920, 1312, 0, 0, 0, "90 bottom keeps source right"},
		{LoadOpts{FitMode: "fill", Rotation: 270, Align: "top"}, 1920, 1312, 0, 0, 0, "270 top keeps source right"},
		{LoadOpts{FitMode: "fill", Rotation: 90, Align: "middle"}, 1920, 656, 656, 0, 0, "90 middle crops evenly"},
		{LoadOpts{FitMode: "fill", Rotation: 90}, 1920, 656, 656, 0, 0, "90 unaligned crops evenly"},
		{LoadOpts{FitMode: "fill", Rotation: 90, Flip: "h", Align: "bottom"}, 1920, 1312, 0, 0, 0, "90+h (transpose) bottom keeps source right"},
		{LoadOpts{FitMode: "fill", Rotation: 270, Flip: "v", Align: "top"}, 1920, 0, 1312, 0, 0, "270+v (transpose) top keeps source left"},
		// Turned 1080x1920 filling the height of a 400x1080 box: 368.9 px of
		// (source-height) overflow; screen left is the source bottom at 90.
		{LoadOpts{FitMode: "fill-height", Rotation: 90, Align: "left"}, 400, 0, 0, 368, 0, "90 left keeps source bottom"},
		{LoadOpts{FitMode: "fill-height", Rotation: 270, Align: "left"}, 400, 0, 0, 0, 368, "270 left keeps source top"},
		// Unturned but mirrored/half-turned: 1920x1080 filling a 1440x1080 box, 480 px.
		{LoadOpts{FitMode: "fill-height", Flip: "h", Align: "left"}, 1440, 480, 0, 0, 0, "h-flip left keeps source right"},
		{LoadOpts{FitMode: "fill-height", Rotation: 180, Align: "right"}, 1440, 0, 480, 0, 0, "180 right keeps source left"},
	} {
		l := computeLayout(c.opts, 1920, 1080, c.bw, 1080, false)
		if l.cropL != c.cl || l.cropR != c.cr || l.cropT != c.ct || l.cropB != c.cb {
			t.Errorf("%s: crop l=%d r=%d t=%d b=%d, want %d %d %d %d", c.desc, l.cropL, l.cropR, l.cropT, l.cropB, c.cl, c.cr, c.ct, c.cb)
		}
	}
	// The software turn's pre-scale applies the same split (scaled by k).
	if l := computeLayout(LoadOpts{FitMode: "fill", Rotation: 90, Align: "top"}, 1920, 1080, 1920, 1080, true); l.cropL != 0 || l.cropR == 0 {
		t.Errorf("90 top, pre-scaled: %+v", l)
	}
	// A 50% box: right/bottom puts it at the far corner, centre in the middle,
	// an explicit X wins.
	if x, y, w, h := wallRect(LoadOpts{GeomW: "50%", GeomH: "50%", Align: "right bottom"}, 1920, 1080); x != 960 || y != 540 || w != 960 || h != 540 {
		t.Errorf("box right bottom: %d,%d %dx%d", x, y, w, h)
	}
	if x, y, _, _ := wallRect(LoadOpts{GeomW: "50%", GeomH: "50%", Align: "center middle"}, 1920, 1080); x != 480 || y != 270 {
		t.Errorf("box centre: %d,%d", x, y)
	}
	if x, _, _, _ := wallRect(LoadOpts{GeomX: "10", GeomW: "50%", Align: "right"}, 1920, 1080); x != 10 {
		t.Errorf("explicit X must win: %d", x)
	}
}

func TestExplicitZeroOpacity(t *testing.T) {
	if got := opacityOf(LoadOpts{Opacity: 0, OpacitySet: true}); got != 0 {
		t.Fatalf("explicit zero became %v", got)
	}
	if got := opacityOf(LoadOpts{}); got != 1 {
		t.Fatalf("default opacity %v", got)
	}
}
