package routes

import (
	"net"
	"testing"
	"time"

	"CuTePi/ctp"
	"CuTePi/media"
)

// SLIP framing: packets split across reads, escaped END/ESC bytes, and
// empty packets between ENDs.
func TestSlipDecoder(t *testing.T) {
	enc := func(addr string) []byte {
		b := append([]byte(addr), 0)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		b = append(b, ',', 0, 0, 0)
		return append(b, slipEND)
	}
	d := &slipDecoder{}
	var pkts [][]byte
	pkts = append(pkts, d.feed([]byte{slipEND})...) // leading END: nothing
	pkts = append(pkts, d.feed(enc("/go"))...)      // whole packet
	half := enc("/stop")
	pkts = append(pkts, d.feed(half[:5])...) // split across reads
	pkts = append(pkts, d.feed(half[5:])...)
	pkts = append(pkts, d.feed([]byte{slipEND})...)                                // empty packet: dropped
	pkts = append(pkts, d.feed([]byte{'a', slipESC, slipESCEnd, 'b', slipEND})...) // escaped END
	if len(pkts) != 3 {
		t.Fatalf("decoded %d packets, want 3 (/go, /stop, escaped)", len(pkts))
	}
	if got := string(pkts[2][:3]); got != "a\xc0b" {
		t.Errorf("escaped packet = %q, want a\\xc0b", got)
	}
}

// End to end over TCP: a SLIP-framed OSC select drives the sheet.
func TestOSCTCPSelectNext(t *testing.T) {
	r := setupTestServer(t)
	_ = r
	for _, f := range []string{"tcp-a.mp4", "tcp-b.mp4"} {
		if err := ctp.RegisterMedia(f, 100, media.Metadata{Mimetype: "video/mp4"}, f); err != nil {
			t.Fatalf("RegisterMedia: %v", err)
		}
		if err := ctp.AddCue(f, ""); err != nil {
			t.Fatalf("AddCue: %v", err)
		}
	}
	if err := ctp.SetCue("1"); err != nil {
		t.Fatalf("SetCue: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			serveOSCTCP(conn)
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	msg := append([]byte("/select/next"), 0)
	for len(msg)%4 != 0 {
		msg = append(msg, 0)
	}
	msg = append(msg, ',', 0, 0, 0, slipEND)
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if pos, _ := ctp.SelectedCuePos(); pos == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("TCP OSC /select/next never advanced the selection")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
