package media

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"strings"
)

// A/V sync test signal (DESIGN §12.11). A 2 s cycle: at its start (the sync
// point) a 1 kHz tone begins, the background flashes white and the green
// corner marker lights, all for AVSyncToneFrames frames. A dial hand sweeps
// once a cycle over a scale numbered in frames, and around the sync point an
// offset meter's cursor shows each frame's own time from the tone's onset, so
// a phone video stepped frame by frame reads the picture-to-sound delay off
// the scale. The clip is generated once per display mode and played through
// the ordinary file path (decode -> wall + audio sink), so it measures what a
// cue would show.
//
// Go draws the picture (the face once, the moving parts per frame, every one
// from the frame index) and pipes raw frames to ffmpeg, which adds the text
// (drawtext, counters from the frame number) and encodes.

const (
	// AVSyncVersion is part of the cached clip's file name: raise it when the
	// signal changes so old clips are not reused.
	AVSyncVersion = 2
	// AVSyncCycle is the cycle length in seconds, AVSyncCycles the number of
	// cycles in the clip; it plays looped.
	AVSyncCycle   = 2
	AVSyncCycles  = 10
	AVSyncSeconds = AVSyncCycle * AVSyncCycles
	// AVSyncToneFrames is the length in frames of the tone, the background
	// flash and the tick markers.
	AVSyncToneFrames = 2
	// AVSyncMeterMs is the offset meter's range either side of the sync point.
	AVSyncMeterMs = 80
	// EBU R37 end-to-end tolerance: sound no more than 40 ms before the
	// picture and no more than 60 ms after it. The meter is red outside it,
	// amber in its outer half and green in its inner half.
	AVSyncEarlyMs = 40
	AVSyncLateMs  = 60

	avRate = 48000 // audio samples a second
)

// The picture is laid out on a 1920x1080 sheet ("design units") and scaled to
// the clip.
const (
	avCX, avCY, avR = 960.0, 590.0, 345.0 // dial centre and outer radius

	avMeterX0, avMeterX1 = 600.0, 1320.0 // offset meter bar: -80 .. +80 ms
	avMeterY0, avMeterY1 = 150.0, 182.0
	avMeterLaneY         = 194.0 // row under the bar that only the cursor's pointer lights

	avTimeX0, avTimeX1 = 300.0, 1620.0 // timeline bar: 0 .. 2 s
	avTimeY0, avTimeY1 = 974.0, 998.0

	avMarkR = 90.0 // corner marker radius

	avProbeX, avProbeY = 300.0, 600.0 // a bare background point (flashes white)
)

type avRGB [3]uint8

var (
	avWhite = avRGB{255, 255, 255}
	avCyan  = avRGB{34, 211, 238}
	avTeal  = avRGB{19, 94, 104}
	avHand  = avRGB{255, 48, 32}
	avGreen = avRGB{31, 170, 74}
	avAmber = avRGB{200, 156, 20}
	avRed   = avRGB{196, 40, 30}
	avPanel = avRGB{14, 14, 14}
)

// avMarks are the corner markers: green (top right) on the sync frames, blue
// (bottom left) for the whole first half of the cycle, white (bottom right)
// on the half-cycle tick frames.
var avMarks = [3]struct {
	x, y float64
	col  avRGB
}{
	{1920 - 40 - avMarkR, 40 + avMarkR, avRGB{0, 255, 60}},
	{40 + avMarkR, 1080 - 40 - avMarkR, avRGB{40, 90, 255}},
	{1920 - 40 - avMarkR, 1080 - 40 - avMarkR, avWhite},
}

// avLayout is one clip's geometry.
type avLayout struct {
	w, h, hz  int
	n         int     // frames in a cycle
	u, ox, oy float64 // design units to pixels: scale and origin
}

func newAVLayout(w, h, hz int) *avLayout {
	l := &avLayout{w: w, h: h, hz: hz, n: AVSyncCycle * hz}
	l.u = math.Min(float64(w)/1920, float64(h)/1080)
	l.ox, l.oy = (float64(w)-1920*l.u)/2, (float64(h)-1080*l.u)/2
	return l
}

// px is the pixel under the design point (x, y).
func (l *avLayout) px(x, y float64) (int, int) {
	return int(l.ox + x*l.u), int(l.oy + y*l.u)
}

// offset is frame f's distance from the sync point in frames: negative in the
// second half of the cycle (before the next sync point).
func (l *avLayout) offset(f int) int {
	if f > l.n/2 {
		return f - l.n
	}
	return f
}

// offsetMs is the time from the tone's onset to the start of frame f.
func (l *avLayout) offsetMs(f int) float64 { return float64(l.offset(f)) * 1000 / float64(l.hz) }

// onMeter reports whether frame f is inside the offset meter's range.
func (l *avLayout) onMeter(f int) bool { return math.Abs(l.offsetMs(f)) <= AVSyncMeterMs }

// meterX is the design x of ms on the offset meter.
func (l *avLayout) meterX(ms float64) float64 {
	return (avMeterX0+avMeterX1)/2 + ms*(avMeterX1-avMeterX0)/(2*AVSyncMeterMs)
}

func (l *avLayout) sync(f int) bool { return f < AVSyncToneFrames }
func (l *avLayout) half(f int) bool { return f >= l.n/2 && f < l.n/2+AVSyncToneFrames }

// marks are the corner markers lit on frame f (the order of avMarks).
func (l *avLayout) marks(f int) [3]bool { return [3]bool{l.sync(f), f < l.n/2, l.half(f)} }

// base is the face under frame f: 0 with the countdown at 2, 1 with it at 1,
// 2 the sync frames' white background.
func (l *avLayout) base(f int) int {
	switch {
	case l.sync(f):
		return 2
	case f < l.n/2:
		return 0
	}
	return 1
}

// avZone is the meter colour for an offset: red outside EBU R37, amber in the
// outer half of its tolerance, green in the inner half.
func avZone(ms float64) avRGB {
	switch {
	case ms < -AVSyncEarlyMs || ms > AVSyncLateMs:
		return avRed
	case ms < -AVSyncEarlyMs/2 || ms > AVSyncLateMs/2:
		return avAmber
	}
	return avGreen
}

// avZones are the meter's zone edges in ms, for drawing.
var avZones = []float64{-AVSyncMeterMs, -AVSyncEarlyMs, -AVSyncEarlyMs / 2, AVSyncLateMs / 2, AVSyncLateMs, AVSyncMeterMs}

// avCanvas is an RGB24 picture drawn in design units, with anti-aliased edges.
type avCanvas struct {
	w, h      int
	pix       []byte
	u, ox, oy float64
	dirty     []image.Rectangle // pixel boxes drawn on since the last reset
}

func (l *avLayout) canvas() *avCanvas {
	return &avCanvas{w: l.w, h: l.h, pix: make([]byte, l.w*l.h*3), u: l.u, ox: l.ox, oy: l.oy}
}

// shade blends col over the design box (x0,y0)-(x1,y1) where sd, the signed
// distance in design units to the shape's edge, is negative: one pixel of
// distance is one step of coverage.
func (c *avCanvas) shade(x0, y0, x1, y1 float64, col avRGB, alpha float64, sd func(x, y float64) float64) {
	r := image.Rect(int(math.Floor(c.ox+x0*c.u))-1, int(math.Floor(c.oy+y0*c.u))-1,
		int(math.Ceil(c.ox+x1*c.u))+1, int(math.Ceil(c.oy+y1*c.u))+1).Intersect(image.Rect(0, 0, c.w, c.h))
	if r.Empty() {
		return
	}
	c.dirty = append(c.dirty, r)
	for py := r.Min.Y; py < r.Max.Y; py++ {
		y := (float64(py) + 0.5 - c.oy) / c.u
		p := c.pix[(py*c.w+r.Min.X)*3:]
		for px := r.Min.X; px < r.Max.X; px++ {
			a := 0.5 - sd((float64(px)+0.5-c.ox)/c.u, y)*c.u
			if a <= 0 {
				continue
			}
			a = math.Min(a, 1) * alpha
			for k, i := 0, (px-r.Min.X)*3; k < 3; k++ {
				p[i+k] = uint8(float64(p[i+k]) + (float64(col[k])-float64(p[i+k]))*a + 0.5)
			}
		}
	}
}

// box fills a rectangle with corner radius rad.
func (c *avCanvas) box(x0, y0, x1, y1, rad float64, col avRGB, alpha float64) {
	mx, my, hx, hy := (x0+x1)/2, (y0+y1)/2, (x1-x0)/2-rad, (y1-y0)/2-rad
	c.shade(x0, y0, x1, y1, col, alpha, func(x, y float64) float64 {
		qx, qy := math.Abs(x-mx)-hx, math.Abs(y-my)-hy
		return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - rad
	})
}

// panel is a dark rounded box with a thin border.
func (c *avCanvas) panel(x0, y0, x1, y1 float64) {
	c.box(x0, y0, x1, y1, 12, avRGB{70, 70, 70}, 1)
	c.box(x0+1.5, y0+1.5, x1-1.5, y1-1.5, 10.5, avPanel, 1)
}

// arc fills the ring between radii r0 and r1 around (cx, cy), from angle a0
// to a1 in degrees clockwise from 12 o'clock (less than 180 apart, or a full
// turn). r0 < 0 gives a disc.
func (c *avCanvas) arc(cx, cy, r0, r1, a0, a1 float64, col avRGB, alpha float64) {
	s0, c0 := math.Sincos(a0 * math.Pi / 180)
	s1, c1 := math.Sincos(a1 * math.Pi / 180)
	full := a1-a0 >= 360
	c.shade(cx-r1, cy-r1, cx+r1, cy+r1, col, alpha, func(x, y float64) float64 {
		x, y = x-cx, y-cy
		d := math.Hypot(x, y)
		sd := math.Max(r0-d, d-r1)
		if !full { // inside is clockwise of a0 and anticlockwise of a1
			sd = math.Max(sd, math.Max(-(s0*y+c0*x), s1*y+c1*x))
		}
		return sd
	})
}

func (c *avCanvas) disc(cx, cy, r float64, col avRGB, alpha float64) {
	c.arc(cx, cy, -1, r, 0, 360, col, alpha)
}

// line draws a round-ended line of the given width.
func (c *avCanvas) line(x0, y0, x1, y1, width float64, col avRGB, alpha float64) {
	dx, dy := x1-x0, y1-y0
	ll := dx*dx + dy*dy
	hw := width / 2
	c.shade(math.Min(x0, x1)-hw, math.Min(y0, y1)-hw, math.Max(x0, x1)+hw, math.Max(y0, y1)+hw, col, alpha, func(x, y float64) float64 {
		t := 0.0
		if ll > 0 {
			t = math.Max(0, math.Min(1, ((x-x0)*dx+(y-y0)*dy)/ll))
		}
		return math.Hypot(x-x0-t*dx, y-y0-t*dy) - hw
	})
}

// spoke draws a line along the dial radius at deg, from radius r0 to r1.
func (c *avCanvas) spoke(deg, r0, r1, width float64, col avRGB, alpha float64) {
	s, co := math.Sincos(deg * math.Pi / 180)
	c.line(avCX+r0*s, avCY-r0*co, avCX+r1*s, avCY-r1*co, width, col, alpha)
}

// tri fills a triangle.
func (c *avCanvas) tri(ax, ay, bx, by, cx, cy float64, col avRGB, alpha float64) {
	p := [4][2]float64{{ax, ay}, {bx, by}, {cx, cy}, {ax, ay}}
	sign := 1.0 // so the inside is the negative side of every edge
	if (bx-ax)*(cy-ay)-(by-ay)*(cx-ax) > 0 {
		sign = -1
	}
	c.shade(math.Min(ax, math.Min(bx, cx)), math.Min(ay, math.Min(by, cy)), math.Max(ax, math.Max(bx, cx)), math.Max(ay, math.Max(by, cy)), col, alpha,
		func(x, y float64) float64 {
			sd := math.Inf(-1)
			for i := 0; i < 3; i++ {
				ex, ey := p[i+1][0]-p[i][0], p[i+1][1]-p[i][1]
				sd = math.Max(sd, sign*(ex*(y-p[i][1])-ey*(x-p[i][0]))/math.Hypot(ex, ey))
			}
			return sd
		})
}

// tickStep is the dial's tick spacing in frames: every frame up to 120 ticks
// a turn, then every second, and so on.
func (l *avLayout) tickStep() int { return (l.n + 119) / 120 }

// labelStep is the dial's label spacing in frames.
func (l *avLayout) labelStep() int {
	s := 20
	for l.n/s > 12 {
		s *= 2
	}
	return s
}

// drawFace draws everything that does not move (the text comes from ffmpeg).
// flash is the sync frames' face: the background white instead of the grid.
func (l *avLayout) drawFace(c *avCanvas, flash bool) {
	// Background: a dark grid, or white.
	bg, minor, major := avRGB{38, 38, 38}, avRGB{52, 52, 52}, avRGB{72, 72, 72}
	if flash {
		bg, minor, major = avWhite, avWhite, avWhite
	}
	for i := 0; i < len(c.pix); i += 3 {
		copy(c.pix[i:], bg[:])
	}
	for k := -64; k <= 128; k++ { // a line every 30 units, a stronger one every 150, from the sheet's centre
		col := minor
		if k%5 == 0 {
			col = major
		}
		if x := int(math.Round(c.ox + (960+float64(k)*30)*c.u)); x >= 0 && x < c.w {
			for y := 0; y < c.h; y++ {
				copy(c.pix[(y*c.w+x)*3:], col[:])
			}
		}
		if y := int(math.Round(c.oy + (540+float64(k)*30)*c.u)); y >= 0 && y < c.h {
			for x := 0; x < c.w; x++ {
				copy(c.pix[(y*c.w+x)*3:], col[:])
			}
		}
	}

	// Info panel (text only) and its divider.
	c.panel(40, 40, 500, 350)
	c.line(64, 206, 476, 206, 1.5, avTeal, 1)

	// Offset meter: the zones, cut into 5 ms segments, and a tick every 20 ms.
	c.panel(570, 40, 1350, 232)
	for i := 0; i+1 < len(avZones); i++ {
		c.box(l.meterX(avZones[i]), avMeterY0, l.meterX(avZones[i+1]), avMeterY1, 0, avZone((avZones[i]+avZones[i+1])/2), 1)
	}
	for ms := -AVSyncMeterMs + 5.0; ms < AVSyncMeterMs; ms += 5 {
		c.line(l.meterX(ms), avMeterY0, l.meterX(ms), avMeterY1, 1.2, avPanel, 0.55)
	}
	for ms := -AVSyncMeterMs; ms <= AVSyncMeterMs; ms += 20 {
		c.line(l.meterX(float64(ms)), avMeterY0-8, l.meterX(float64(ms)), avMeterY0-2, 2, avRGB{170, 170, 170}, 1)
	}

	// Timeline: the track and its 0, 1 and 2 s ticks.
	c.panel(260, 950, 1660, 1045)
	c.box(avTimeX0, avTimeY0, avTimeX1, avTimeY1, 0, avRGB{58, 58, 58}, 1)
	for i := 1; i < 40; i++ {
		x := avTimeX0 + (avTimeX1-avTimeX0)*float64(i)/40
		c.line(x, avTimeY0, x, avTimeY1, 1.2, avPanel, 0.7)
	}
	for _, x := range []float64{avTimeX0, (avTimeX0 + avTimeX1) / 2, avTimeX1} {
		c.line(x, avTimeY1+2, x, avTimeY1+10, 2.5, avWhite, 1)
	}

	// Corner markers, unlit: a dark disc in a white ring.
	for _, m := range avMarks {
		c.disc(m.x, m.y, avMarkR, avRGB{225, 225, 225}, 1)
		c.disc(m.x, m.y, avMarkR-4, avRGB{8, 8, 8}, 1)
	}

	// Dial, from the outside in: bezel with the tolerance arc at 0, white
	// ring, frame labels (text), tick ring, inner rings and cross.
	c.disc(avCX, avCY, avR, avRGB{125, 125, 125}, 1)
	c.disc(avCX, avCY, avR-3, avRGB{10, 10, 10}, 1)
	deg := 360.0 / (AVSyncCycle * 1000) // degrees a millisecond
	for i := 0; i+1 < len(avZones); i++ {
		c.arc(avCX, avCY, avR-36, avR-9, avZones[i]*deg, avZones[i+1]*deg, avZone((avZones[i]+avZones[i+1])/2), 1)
	}
	for a := 30.0; a < 360; a += 30 {
		if a != 180 { // the half-cycle "1" is there
			c.spoke(a, avR-13, avR-6, 2, avRGB{120, 120, 120}, 1)
		}
	}
	c.disc(avCX, avCY, avR-42, avRGB{228, 228, 228}, 1)
	c.disc(avCX, avCY, avR-46, avRGB{18, 18, 18}, 1)
	for _, r := range []float64{190, 128, 64} {
		c.arc(avCX, avCY, r-0.75, r+0.75, 0, 360, avTeal, 1)
	}
	c.line(avCX-252, avCY, avCX+252, avCY, 1.5, avTeal, 1)
	c.line(avCX, avCY-252, avCX, avCY+252, 1.5, avTeal, 1)
	c.arc(avCX, avCY, 251, 253, 0, 360, avRGB{150, 150, 150}, 1)
	for f := 0; f < l.n; f += l.tickStep() {
		a := 360 * float64(f) / float64(l.n)
		switch {
		case f == 0:
			c.spoke(a, 234, 251, 4, avRGB{60, 220, 110}, 1)
		case f%10 == 0:
			c.spoke(a, 214, 251, 3, avCyan, 1)
		case f%5 == 0:
			c.spoke(a, 231, 251, 2, avRGB{225, 225, 225}, 1)
		default:
			c.spoke(a, 238, 251, 2, avRGB{190, 190, 190}, 1)
		}
	}
	l.lamp(c, avRGB{70, 70, 70})
}

// lamp draws the sync marker, a pointer at the dial's 0.
func (l *avLayout) lamp(c *avCanvas, col avRGB) {
	c.tri(avCX-17, avCY-294, avCX+17, avCY-294, avCX, avCY-262, col, 1)
}

// drawMoving draws frame f's moving parts over the face.
func (l *avLayout) drawMoving(c *avCanvas, f int) {
	// Timeline: the elapsed part and the cursor.
	x := avTimeX0 + (avTimeX1-avTimeX0)*float64(f)/float64(l.n)
	c.box(avTimeX0, avTimeY0, x, avTimeY1, 0, avRGB{170, 40, 28}, 0.85)
	c.line(x, avTimeY0-6, x, avTimeY1+4, 4, avCyan, 1)
	c.tri(x-9, avTimeY0-18, x+9, avTimeY0-18, x, avTimeY0-5, avCyan, 1)

	// Offset meter cursor: this frame's time from the tone's onset.
	if l.onMeter(f) {
		x := l.meterX(l.offsetMs(f))
		c.line(x, avMeterY0-3, x, avMeterY1+4, 9, avPanel, 1)
		c.line(x, avMeterY0-3, x, avMeterY1+4, 4.5, avWhite, 1)
		c.tri(x-10, avMeterY1+19, x+10, avMeterY1+19, x, avMeterY1+5, avWhite, 1)
	}

	for i, on := range l.marks(f) {
		if on {
			c.disc(avMarks[i].x, avMarks[i].y, avMarkR, avMarks[i].col, 1)
		}
	}
	if l.sync(f) {
		l.lamp(c, avWhite)
	}

	// Hand, and its pointer on the bezel (it crosses the tolerance arc).
	a := 360 * float64(f) / float64(l.n)
	c.spoke(a, avR-39, avR-6, 3.5, avWhite, 1)
	c.spoke(a, 0, 244, 4.5, avHand, 1)
	c.disc(avCX, avCY, 9, avHand, 1)
}

// avFont is drawtext's font option: DejaVu Sans Mono, or whatever fontconfig
// calls monospace where it is not installed.
func avFont(bold bool) string {
	f := "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
	if bold {
		f = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono-Bold.ttf"
	}
	if _, err := os.Stat(f); err != nil {
		return "font=monospace"
	}
	return "fontfile=" + f
}

// avEscape makes a plain string safe as drawtext text inside single quotes.
var avEscape = strings.NewReplacer(`\`, `\\`, `:`, `\:`, `%`, `\%`)

const (
	avLeft = iota
	avCentre
	avRight
)

// text is a drawtext filter: s (drawtext syntax) with its baseline at the
// design point (x, y), shown while enable holds (always when empty).
func (l *avLayout) text(s string, x, y, size float64, col string, bold bool, align int, enable string) string {
	xe := fmt.Sprintf("%.1f", l.ox+x*l.u)
	switch align {
	case avCentre:
		xe += "-tw/2"
	case avRight:
		xe += "-tw"
	}
	f := fmt.Sprintf("drawtext=%s:text='%s':fontsize=%.1f:fontcolor=%s:x=%s:y=%.1f:y_align=baseline",
		avFont(bold), s, size*l.u, col, xe, l.oy+y*l.u)
	if enable != "" {
		f += ":enable='" + enable + "'"
	}
	return f
}

// mid is the baseline that centres digits of the given size on y (DejaVu's
// digits are 0.73 em tall).
func mid(y, size float64) float64 { return y + 0.365*size }

const (
	avTxtWhite = "white"
	avTxtGrey  = "0x9c9c9c"
	avTxtCyan  = "0x22d3ee"
	avTxtGreen = "0x3cdc6e"
)

// staticText is the text of the three faces (frame i of the text pass is
// face i, see base).
func (l *avLayout) staticText() []string {
	t := []string{
		// Info panel.
		l.text(fmt.Sprintf("%d×%d", l.w, l.h), 64, 240, 23, avTxtGrey, false, avLeft, ""),
		l.text(avEscape.Replace("BT.709 / SDR"), 64, 300, 23, avTxtGrey, false, avLeft, ""),
		l.text(fmt.Sprintf("%d Hz", l.hz), 64, 330, 23, avTxtGrey, false, avLeft, ""),
		// Offset meter.
		l.text("REMAINING", avMeterX0, 70, 17, avTxtGrey, false, avLeft, ""),
		l.text("OFFSET", 960, 70, 17, avTxtGrey, false, avCentre, ""),
		l.text("FRAMES", avMeterX1, 70, 17, avTxtGrey, false, avRight, ""),
		l.text("◀ AUDIO EARLY", avMeterX0, 220, 16, avTxtGrey, false, avLeft, ""),
		l.text("EBU R37", 960, 220, 16, avTxtGreen, true, avCentre, ""),
		l.text("AUDIO LATE ▶", avMeterX1, 220, 16, avTxtGrey, false, avRight, ""),
		// Timeline.
		l.text("0s", avTimeX0, 1034, 21, avTxtWhite, true, avCentre, ""),
		l.text("1s", (avTimeX0+avTimeX1)/2, 1034, 21, avTxtGrey, true, avCentre, ""),
		l.text("2s", avTimeX1, 1034, 21, avTxtWhite, true, avCentre, ""),
		// Corner markers.
		l.text("SYNC", avMarks[0].x, avMarks[0].y+avMarkR+30, 19, avTxtGrey, true, avCentre, ""),
		l.text("0–1s", avMarks[1].x, avMarks[1].y-avMarkR-16, 19, avTxtGrey, true, avCentre, ""),
		l.text("1s", avMarks[2].x, avMarks[2].y-avMarkR-16, 19, avTxtGrey, true, avCentre, ""),
		// Dial centre: the countdown (2 on faces 0 and 2, 1 on face 1) and the title.
		l.text("2", avCX, mid(avCY-80, 170), 170, "0x5c5c5c", true, avCentre, `eq(n\,0)+eq(n\,2)`),
		l.text("1", avCX, mid(avCY-80, 170), 170, "0x5c5c5c", true, avCentre, `eq(n\,1)`),
		l.text("AV SYNC", avCX, avCY+72, 32, avTxtWhite, true, avCentre, ""),
		l.text(fmt.Sprintf("%ds cycle", AVSyncCycle), avCX, avCY+104, 20, avTxtGrey, false, avCentre, ""),
		l.text(fmt.Sprintf("%d Hz", l.hz), avCX, avCY+134, 20, avTxtCyan, false, avCentre, ""),
		// Dial: 0 under its tick, the half-cycle second on the bezel.
		l.text("0", avCX, mid(avCY-212, 24), 24, avTxtGreen, true, avCentre, ""),
		l.text("1", avCX, mid(avCY+avR-24, 30), 30, avTxtWhite, true, avCentre, ""),
	}
	for ms := -AVSyncMeterMs; ms <= AVSyncMeterMs; ms += 20 {
		s, col := fmt.Sprintf("%+d", ms), "0xd0d0d0"
		if ms == 0 {
			s, col = "0", avTxtGreen
		}
		t = append(t, l.text(strings.Replace(s, "-", "−", 1), l.meterX(float64(ms)), 136, 18, col, false, avCentre, ""))
	}
	for f := l.labelStep(); f < l.n; f += l.labelStep() {
		s, co := math.Sincos(2 * math.Pi * float64(f) / float64(l.n))
		t = append(t, l.text(fmt.Sprint(f), avCX+277*s, mid(avCY-277*co, 21), 21, avTxtCyan, false, avCentre, ""))
	}
	return t
}

// movingText is the text that changes with the frame number n.
func (l *avLayout) movingText() []string {
	N, H := l.n, l.hz
	f := fmt.Sprintf(`mod(n\,%d)`, N)
	eif := func(expr string, width int) string {
		if width > 0 {
			return fmt.Sprintf(`%%{eif\:%s\:d\:%d}`, expr, width)
		}
		return fmt.Sprintf(`%%{eif\:%s\:d}`, expr)
	}
	tenths := fmt.Sprintf(`round(%s*10000/%d)`, f, H)
	t := []string{
		// Timecode HH:MM:SS:FF.
		l.text(eif(fmt.Sprintf(`floor(n/%d)`, H*3600), 2)+`\:`+eif(fmt.Sprintf(`mod(floor(n/%d)\,60)`, H*60), 2)+`\:`+
			eif(fmt.Sprintf(`mod(floor(n/%d)\,60)`, H), 2)+`\:`+eif(fmt.Sprintf(`mod(n\,%d)`, H), len(fmt.Sprint(H-1))),
			64, 100, 46, avTxtWhite, true, avLeft, ""),
		l.text("Frame "+eif(f, 3)+fmt.Sprintf("/%d", N), 64, 148, 28, avTxtWhite, true, avLeft, ""),
		l.text("MS "+eif("floor("+tenths+"/10)", 0)+"."+eif("mod("+tenths+`\,10)`, 0), 64, 186, 28, avTxtWhite, true, avLeft, ""),
		l.text("Cycle "+eif(fmt.Sprintf(`floor(n/%d)+1`, N), 2)+fmt.Sprintf("/%02d", AVSyncCycles), 64, 270, 23, avTxtGrey, false, avLeft, ""),
		// Time and frames to the next sync point.
		l.text(eif(fmt.Sprintf(`round((%d-%s)*1000/%d)`, N, f, H), 0)+" ms", avMeterX0, 104, 27, avTxtCyan, false, avLeft, ""),
		l.text(eif(fmt.Sprintf(`%d-%s`, N, f), 0)+" frames", avMeterX1, 104, 27, avTxtCyan, false, avRight, ""),
	}
	// The offset readout: one text per frame on the meter, in its zone's colour.
	on := 0
	for f := 0; f < N; f++ {
		if !l.onMeter(f) {
			continue
		}
		on++
		ms := l.offsetMs(f)
		s := strings.Replace(fmt.Sprintf("%+.1f ms", ms), "-", "−", 1)
		if f == 0 {
			s = "0.0 ms"
		}
		z := avZone(ms)
		t = append(t, l.text(s, 960, 106, 31, fmt.Sprintf("0x%02x%02x%02x", z[0]+40, z[1]+40, z[2]+40), true, avCentre,
			fmt.Sprintf(`eq(mod(n\,%d)\,%d)`, N, f)))
	}
	// (the meter's frames are the first on/2+1 and the last on/2 of the cycle)
	return append(t, l.text("· · ·", 960, 106, 31, "0x5c5c5c", true, avCentre,
		fmt.Sprintf(`between(mod(n\,%d)\,%d\,%d)`, N, on/2+1, N-on/2-1)))
}

// avToYUV converts box r of an RGB24 picture into a planar 4:2:0 frame
// (BT.709, limited range). w and h are even.
func avToYUV(rgb, yuv []byte, w, h int, r image.Rectangle) {
	r.Min.X, r.Min.Y, r.Max.X, r.Max.Y = r.Min.X&^1, r.Min.Y&^1, (r.Max.X+1)&^1, (r.Max.Y+1)&^1
	cb, cr := yuv[w*h:], yuv[w*h+w*h/4:]
	for y := r.Min.Y; y < r.Max.Y; y += 2 {
		for x := r.Min.X; x < r.Max.X; x += 2 {
			var sr, sg, sb int
			for _, i := range [4]int{y*w + x, y*w + x + 1, (y+1)*w + x, (y+1)*w + x + 1} {
				R, G, B := int(rgb[i*3]), int(rgb[i*3+1]), int(rgb[i*3+2])
				yuv[i] = uint8((47*R+157*G+16*B+128)>>8 + 16)
				sr, sg, sb = sr+R, sg+G, sb+B
			}
			i := y/2*(w/2) + x/2
			cb[i] = uint8((-26*sr-86*sg+112*sb+512)>>10 + 128)
			cr[i] = uint8((112*sr-102*sg-10*sb+512)>>10 + 128)
		}
	}
}

// avFFmpeg runs ffmpeg with write feeding its standard input.
func avFFmpeg(write func(io.Writer) error, stdout io.Writer, args ...string) error {
	cmd, cancel := toolCmd(ThumbnailTimeout*6, "ffmpeg", append([]string{"-y", "-v", "error"}, args...)...)
	defer cancel()
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := write(stdin)
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("media: generating the A/V sync clip: %w: %s", err, stderr.Bytes())
	}
	return werr
}

// avAudio is the lavfi source for the clip's sound, counted in samples so the
// onsets are exact: 1 kHz at half scale from the sync point, 500 Hz at
// quarter scale from the half cycle, each AVSyncToneFrames frames long, the
// same on both channels.
func (l *avLayout) avAudio() string {
	cycle, tone := AVSyncCycle*avRate, int(math.Round(AVSyncToneFrames*avRate/float64(l.hz)))
	m := fmt.Sprintf(`mod(n\,%d)`, cycle)
	return fmt.Sprintf(`aevalsrc='if(lt(%s\,%d)\,0.5*sin(2*PI*1000*%s/%d)\,if(between(%s\,%d\,%d)\,0.25*sin(2*PI*500*(%s-%d)/%d)\,0))':c=stereo:s=%d:d=%d`,
		m, tone, m, avRate, m, cycle/2, cycle/2+tone-1, m, cycle/2, avRate, avRate, AVSyncSeconds)
}

// GenerateAVSync writes the test clip for a w x h display at hz frames a
// second to path (H.264 with PCM audio in Matroska: no audio encoder priming
// to offset the tone, and the Pi's hardware H.264 decoder at up to 1080p).
func GenerateAVSync(path string, w, h, hz int) error {
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}
	if hz <= 0 {
		hz = 60
	}
	// Above 1080p the H.264 hardware decoder cannot take it; the display
	// layer scales a 1080p picture up for free.
	if w*h > 1920*1080 {
		w, h = 1920, 1080
	}
	w, h = w&^1, h&^1 // 4:2:0
	l := newAVLayout(w, h, hz)
	size := fmt.Sprintf("%dx%d", w, h)

	// The three faces, then their text from ffmpeg (once, not per frame).
	n := w * h * 3
	raw := make([]byte, 0, 3*n)
	for i := 0; i < 3; i++ {
		c := l.canvas()
		l.drawFace(c, i == 2)
		raw = append(raw, c.pix...)
	}
	var faces bytes.Buffer
	if err := avFFmpeg(func(in io.Writer) error { _, err := in.Write(raw); return err }, &faces,
		"-f", "rawvideo", "-pix_fmt", "rgb24", "-video_size", size, "-i", "pipe:0",
		"-vf", strings.Join(l.staticText(), ","), "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1"); err != nil {
		return err
	}
	if faces.Len() != 3*n {
		return fmt.Errorf("media: generating the A/V sync clip: the text pass returned %d bytes, want %d", faces.Len(), 3*n)
	}
	var rgb, yuv [3][]byte
	for i := range rgb {
		rgb[i] = faces.Bytes()[i*n : (i+1)*n]
		yuv[i] = make([]byte, w*h*3/2)
		avToYUV(rgb[i], yuv[i], w, h, image.Rect(0, 0, w, h))
	}

	// The frames: the face, with the moving parts redrawn and converted only
	// where they are.
	frames := func(out io.Writer) error {
		work, frame, cur := l.canvas(), make([]byte, w*h*3/2), -1
		for i := 0; i < AVSyncSeconds*hz; i++ {
			f := i % l.n
			b := l.base(f)
			if b != cur {
				copy(work.pix, rgb[b])
				cur = b
			}
			work.dirty = work.dirty[:0]
			l.drawMoving(work, f)
			copy(frame, yuv[b])
			for _, r := range work.dirty {
				avToYUV(work.pix, frame, w, h, r)
			}
			if _, err := out.Write(frame); err != nil {
				return err
			}
			for _, r := range work.dirty { // back to the bare face
				for y := r.Min.Y; y < r.Max.Y; y++ {
					copy(work.pix[(y*w+r.Min.X)*3:(y*w+r.Max.X)*3], rgb[b][(y*w+r.Min.X)*3:])
				}
			}
		}
		return nil
	}
	tmp := path + ".part"
	bt709 := []string{"-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv"}
	args := append([]string{"-f", "rawvideo", "-pix_fmt", "yuv420p", "-video_size", size, "-framerate", fmt.Sprint(hz)}, bt709...)
	args = append(args, "-i", "pipe:0", "-f", "lavfi", "-i", l.avAudio(),
		"-vf", strings.Join(l.movingText(), ","),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "18", "-g", fmt.Sprint(hz), "-pix_fmt", "yuv420p")
	args = append(args, bt709...)
	args = append(args, "-c:a", "pcm_s16le", "-f", "matroska", tmp)
	if err := avFFmpeg(frames, nil, args...); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
