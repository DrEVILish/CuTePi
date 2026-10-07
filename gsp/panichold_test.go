package gsp

import "testing"

// Without a KMS wall (tests run on fakesink) nothing is armed, and a panic
// must report the miss so the caller takes the cold load path.
func TestPanicHoldFallsBackWhenNotArmed(t *testing.T) {
	ArmPanicHold("hold.png")
	standby.mu.Lock()
	armed := standby.p != nil
	standby.mu.Unlock()
	if armed {
		t.Fatal("armed without a KMS wall")
	}
	if PanicToHold("hold.png") {
		t.Fatal("PanicToHold claimed a cut with nothing armed")
	}
	ArmPanicHold("") // disarm is a no-op when idle
}
