package media

// Animated images (DESIGN §6.1.3): an animated GIF, APNG, WebP, AVIF or JPEG
// XL plays as a timeline, not as a still. Whether a file is animated is read
// from its header — the GIF's second image descriptor, the APNG acTL chunk,
// the WebP VP8X animation flag, the AVIF "avis" (image sequence) brand, the
// JPEG XL image header's have_animation — so playback can decide in
// microseconds, without a probe on the GO path. The extension never decides: a .gif is often a
// single frame, and an APNG is often named .png.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"image"
	"image/gif"
	"io"
	"os"
)

// Animation describes an image file's animation.
type Animation struct {
	Animated bool // more than one frame
	// LoopForever: the file asks to repeat without end (GIF NETSCAPE2.0 loop
	// count 0, APNG num_plays 0, WebP ANIM loop count 0). Otherwise it plays
	// Loops times in all (1 when the file says nothing).
	LoopForever bool
	Loops       int
}

// ImageAnimation reads path's header. Files that are not GIF, PNG, WebP,
// AVIF or JPEG XL, or that cannot be read, report a still.
func ImageAnimation(path string) Animation {
	f, err := os.Open(path)
	if err != nil {
		return Animation{}
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	head, err := r.Peek(16)
	if err != nil && len(head) < 12 {
		return Animation{}
	}
	var a Animation
	switch {
	case bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a")):
		a = gifAnimation(r)
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		a = pngAnimation(r)
	case bytes.HasPrefix(head, []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		a = webpAnimation(r)
	case bytes.Equal(head[4:8], []byte("ftyp")):
		a = avifAnimation(r)
	case bytes.HasPrefix(head, []byte{0xff, 0x0a}) || bytes.HasPrefix(head, jxlBox):
		a = jxlAnimation(r)
	}
	if !a.Animated {
		return Animation{} // a still: no loop count
	}
	return a
}

// gifAnimation walks the GIF block structure until a second image
// descriptor (animated) or the trailer, noting the NETSCAPE2.0 loop count.
func gifAnimation(r *bufio.Reader) Animation {
	a := Animation{Loops: 1}
	hdr := make([]byte, 13) // signature + logical screen descriptor
	if _, err := io.ReadFull(r, hdr); err != nil {
		return Animation{}
	}
	if hdr[10]&0x80 != 0 { // global colour table
		if _, err := r.Discard(3 << (uint(hdr[10]&7) + 1)); err != nil {
			return Animation{}
		}
	}
	frames := 0
	for {
		b, err := r.ReadByte()
		if err != nil {
			return a
		}
		switch b {
		case 0x21: // extension: label, then sub-blocks
			label, err := r.ReadByte()
			if err != nil {
				return a
			}
			if label == 0xFF { // application extension: NETSCAPE2.0 loop count
				n, _ := r.ReadByte()
				id := make([]byte, n)
				if _, err := io.ReadFull(r, id); err != nil {
					return a
				}
				if string(id) == "NETSCAPE2.0" {
					if sz, _ := r.ReadByte(); sz >= 3 {
						sub := make([]byte, sz)
						if _, err := io.ReadFull(r, sub); err != nil {
							return a
						}
						if sub[0] == 1 {
							loops := int(binary.LittleEndian.Uint16(sub[1:3]))
							a.LoopForever = loops == 0
							a.Loops = loops + 1 // the count is of repeats
						}
					} else if sz == 0 {
						continue
					}
				}
			}
			if !skipSubBlocks(r) {
				return a
			}
		case 0x2C: // image descriptor
			frames++
			if frames > 1 {
				a.Animated = true
				return a
			}
			d := make([]byte, 9)
			if _, err := io.ReadFull(r, d); err != nil {
				return a
			}
			if d[8]&0x80 != 0 { // local colour table
				if _, err := r.Discard(3 << (uint(d[8]&7) + 1)); err != nil {
					return a
				}
			}
			if _, err := r.ReadByte(); err != nil { // LZW minimum code size
				return a
			}
			if !skipSubBlocks(r) {
				return a
			}
		default: // 0x3B trailer, or not a valid block
			return a
		}
	}
}

// GIFTransparent reports whether a GIF's first frame shows any transparent
// pixel. ffmpeg decodes every GIF to BGRA, so the pixel format cannot tell
// an opaque GIF from a transparent one, and the transparency flag cannot
// either: encoders reserve a transparent colour in every frame (ffmpeg's
// palettegen does by default) and use it in later frames for pixels that do
// not change. The first frame's pixels decide; a first frame smaller than
// the canvas leaves the rest transparent.
func GIFTransparent(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	cfg, err := gif.DecodeConfig(f)
	if err != nil {
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	img, err := gif.Decode(bufio.NewReader(f))
	if err != nil {
		return false
	}
	pal, ok := img.(*image.Paletted)
	if !ok {
		return false
	}
	if b := pal.Bounds(); b.Dx() < cfg.Width || b.Dy() < cfg.Height {
		return true
	}
	clear := make([]bool, len(pal.Palette))
	any := false
	for i, c := range pal.Palette {
		if _, _, _, a := c.RGBA(); a < 0xffff {
			clear[i], any = true, true
		}
	}
	if !any {
		return false
	}
	for _, ix := range pal.Pix {
		if int(ix) < len(clear) && clear[ix] {
			return true
		}
	}
	return false
}

func skipSubBlocks(r *bufio.Reader) bool {
	for {
		n, err := r.ReadByte()
		if err != nil {
			return false
		}
		if n == 0 {
			return true
		}
		if _, err := r.Discard(int(n)); err != nil {
			return false
		}
	}
}

// pngAnimation looks for an acTL chunk before the first IDAT (APNG).
func pngAnimation(r *bufio.Reader) Animation {
	if _, err := r.Discard(8); err != nil {
		return Animation{}
	}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return Animation{}
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		switch string(hdr[4:8]) {
		case "acTL":
			d := make([]byte, 8)
			if n < 8 {
				return Animation{}
			}
			if _, err := io.ReadFull(r, d); err != nil {
				return Animation{}
			}
			frames, plays := binary.BigEndian.Uint32(d[:4]), binary.BigEndian.Uint32(d[4:])
			if frames < 2 {
				return Animation{}
			}
			return Animation{Animated: true, LoopForever: plays == 0, Loops: int(max(plays, 1))}
		case "IDAT", "IEND":
			return Animation{}
		}
		if _, err := r.Discard(int(n) + 4); err != nil { // data + CRC
			return Animation{}
		}
	}
}

// webpAnimation reads the VP8X animation flag and the ANIM loop count.
func webpAnimation(r *bufio.Reader) Animation {
	if _, err := r.Discard(12); err != nil {
		return Animation{}
	}
	hdr := make([]byte, 8)
	animated := false
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return Animation{Animated: animated, Loops: 1}
		}
		n := binary.LittleEndian.Uint32(hdr[4:8])
		pad := int(n & 1)
		switch string(hdr[:4]) {
		case "VP8X":
			d := make([]byte, n)
			if _, err := io.ReadFull(r, d); err != nil || len(d) < 1 {
				return Animation{}
			}
			if d[0]&0x02 == 0 {
				return Animation{}
			}
			animated = true
			r.Discard(pad)
			continue
		case "ANIM":
			d := make([]byte, n)
			if _, err := io.ReadFull(r, d); err != nil || len(d) < 6 {
				return Animation{Animated: animated, Loops: 1}
			}
			loops := int(binary.LittleEndian.Uint16(d[4:6]))
			return Animation{Animated: animated, LoopForever: loops == 0, Loops: max(loops, 1)}
		default:
			return Animation{Animated: animated, Loops: 1}
		}
	}
}

// avifAnimation: an AVIF whose ftyp names the "avis" brand (major or
// compatible) is an image sequence. Its repetition is in an edit list deep in
// the movie box; as browsers, a sequence without one repeats forever.
func avifAnimation(r *bufio.Reader) Animation {
	head, _ := r.Peek(8)
	if len(head) < 8 {
		return Animation{}
	}
	size := int(binary.BigEndian.Uint32(head))
	if size < 16 || size > 4096 {
		return Animation{}
	}
	box, err := r.Peek(size)
	if err != nil {
		return Animation{}
	}
	for i := 8; i+4 <= size; i += 4 {
		if i == 12 { // minor version
			continue
		}
		if string(box[i:i+4]) == "avis" {
			return Animation{Animated: true, LoopForever: true, Loops: 1}
		}
	}
	return Animation{}
}

// jxlBox is the signature box that starts a JPEG XL file in the container
// format (a bare codestream starts FF 0A).
var jxlBox = []byte{0, 0, 0, 0x0c, 'J', 'X', 'L', ' ', 0x0d, 0x0a, 0x87, 0x0a}

// jxlAnimation reads the codestream's image header (ISO/IEC 18181-1: the
// SizeHeader, then ImageMetadata up to its AnimationHeader) for
// have_animation and the loop count.
func jxlAnimation(r *bufio.Reader) Animation {
	cs, err := jxlCodestreamStart(r)
	if err != nil || len(cs) < 2 || cs[0] != 0xff || cs[1] != 0x0a {
		return Animation{}
	}
	b := &bitReader{data: cs[2:]}
	jxlSize(b, false) // the image size
	if b.bool() {     // all_default: no animation
		return Animation{}
	}
	if !b.bool() { // extra_fields absent: no animation
		return Animation{}
	}
	b.bits(3) // orientation
	if b.bool() {
		jxlSize(b, false) // intrinsic size
	}
	if b.bool() {
		jxlSize(b, true) // preview
	}
	if !b.bool() || b.err { // have_animation
		return Animation{}
	}
	b.u32([4][2]int{{0, 100}, {0, 1000}, {10, 1}, {30, 1}}) // ticks per second, numerator
	b.u32([4][2]int{{0, 1}, {0, 1001}, {8, 1}, {10, 1}})    // denominator
	loops := b.u32([4][2]int{{0, 0}, {3, 0}, {16, 0}, {32, 0}})
	if b.err {
		return Animation{}
	}
	return Animation{Animated: true, LoopForever: loops == 0, Loops: max(int(loops), 1)}
}

// jxlCodestreamStart returns the first bytes of the codestream: the file
// itself for a bare codestream, else the payload of the container's first
// jxlc or jxlp box (a jxlp payload starts with a 4-byte index).
func jxlCodestreamStart(r *bufio.Reader) ([]byte, error) {
	head, _ := r.Peek(2)
	if bytes.Equal(head, []byte{0xff, 0x0a}) {
		return r.Peek(256)
	}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil, err
		}
		size := int64(binary.BigEndian.Uint32(hdr))
		skip := size - 8
		if size == 1 { // 64-bit size
			ext := make([]byte, 8)
			if _, err := io.ReadFull(r, ext); err != nil {
				return nil, err
			}
			skip = int64(binary.BigEndian.Uint64(ext)) - 16
		}
		switch string(hdr[4:8]) {
		case "jxlc":
			return r.Peek(256)
		case "jxlp":
			if _, err := r.Discard(4); err != nil {
				return nil, err
			}
			return r.Peek(256)
		}
		if size == 0 || skip < 0 {
			return nil, io.ErrUnexpectedEOF
		}
		if _, err := r.Discard(int(skip)); err != nil {
			return nil, err
		}
	}
}

// jxlSize skips a SizeHeader, or a PreviewHeader: the height, a 3-bit aspect
// ratio, and the width only when that ratio is 0.
func jxlSize(b *bitReader, preview bool) {
	dim := func() {} // one stored dimension
	switch {
	case preview && b.bool(): // div8
		dim = func() { b.u32([4][2]int{{0, 16}, {0, 32}, {5, 1}, {9, 33}}) }
	case preview:
		dim = func() { b.u32([4][2]int{{6, 1}, {8, 65}, {10, 321}, {12, 1345}}) }
	case b.bool(): // small: multiples of 8 up to 256
		dim = func() { b.bits(5) }
	default:
		dim = func() { b.u32([4][2]int{{9, 1}, {13, 1}, {18, 1}, {30, 1}}) }
	}
	dim()
	if b.bits(3) == 0 {
		dim()
	}
}

// bitReader reads JPEG XL's least-significant-bit-first fields.
type bitReader struct {
	data []byte
	pos  int // in bits
	err  bool
}

func (b *bitReader) bits(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if b.pos/8 >= len(b.data) {
			b.err = true
			return 0
		}
		v |= uint64(b.data[b.pos/8]>>(b.pos%8)&1) << i
		b.pos++
	}
	return v
}

func (b *bitReader) bool() bool { return b.bits(1) == 1 }

// u32 reads a U32 field: a 2-bit selector, then that distribution's
// (bits, offset): Val(offset) when bits is 0.
func (b *bitReader) u32(d [4][2]int) uint64 {
	c := d[b.bits(2)]
	return b.bits(c[0]) + uint64(c[1])
}
