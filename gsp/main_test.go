package gsp

import (
	"os"
	"testing"
)

// Tests never drive the real wall: a display sink would contend with the
// running service for the HDMI output (and hang in state changes). Tests that
// need a particular sink set CUTEPI_WALL_SINK themselves.
func TestMain(m *testing.M) {
	if os.Getenv("CUTEPI_WALL_SINK") == "" {
		os.Setenv("CUTEPI_WALL_SINK", "fakesink")
	}
	os.Exit(m.Run())
}
