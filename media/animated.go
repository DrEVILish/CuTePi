package media

// Animated images (DESIGN §6.1.3): an animated GIF, APNG or WebP plays as a
// timeline, not as a still. Whether a file is animated is read from its
// header — the GIF's second image descriptor, the APNG acTL chunk, the WebP
// VP8X animation flag — so playback can decide in microseconds, without a
// probe on the GO path. The extension never decides: a .gif is often a
// single frame, and an APNG is often named .png.

import (
	"bufio"
	"bytes"
	"encoding/binary"
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

// ImageAnimation reads path's header. Files that are not GIF, PNG or WebP,
// or that cannot be read, report a still.
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
