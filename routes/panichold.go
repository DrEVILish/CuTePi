package routes

// Keeps the panic holding image armed on the wall (gsp/panichold.go) in step
// with the setting. The image can change behind the settings route — media
// delete clears it, a show import replaces the state, an upload can replace
// the file — so a reconciler re-checks every two seconds (one state read and
// one stat; arming is a no-op while nothing changed), and is kicked at once
// after the setting changes or a panic consumes the armed copy.

import (
	"time"

	"CuTePi/ctp"
	"CuTePi/gsp"
)

var panicArmKickCh = make(chan struct{}, 1)

// kickPanicArm asks the reconciler to re-check now.
func kickPanicArm() {
	select {
	case panicArmKickCh <- struct{}{}:
	default:
	}
}

// RunPanicStandby keeps the holding image armed. Blocks: run with go.
func RunPanicStandby() {
	for {
		gsp.ArmPanicHold(ctp.GetPanicHoldImage())
		select {
		case <-panicArmKickCh:
		case <-time.After(2 * time.Second):
		}
	}
}
