package gsp

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"CuTePi/logs"
)

// Linux console ioctl (linux/kd.h); not exported by x/sys/unix.
const (
	kdSetMode  = 0x4B3A
	kdText     = 0
	kdGraphics = 1
)

// ClaimWallConsole switches the virtual terminal sharing the HDMI output to
// graphics mode: the kernel then stops drawing anything on it — login prompt,
// kernel messages, the blinking cursor — so the wall shows only what CuTePi
// puts there (§6.9 "Black means black"). The framebuffer is cleared to black.
// The returned func restores text mode for a clean shutdown.
func ClaimWallConsole() (release func()) {
	release = func() {}
	tty := os.Getenv("CUTEPI_TTY")
	if tty == "" {
		active, err := os.ReadFile("/sys/class/tty/tty0/active")
		if err != nil {
			logs.Printf(logs.GSPStopErr, "gsp: wall console: %v", err)
			return
		}
		tty = "/dev/" + strings.TrimSpace(string(active))
	}
	f, err := os.OpenFile(tty, os.O_RDWR, 0)
	if err != nil {
		logs.Printf(logs.GSPStopErr, "gsp: wall console %s: %v", tty, err)
		return
	}
	if err := unix.IoctlSetInt(int(f.Fd()), kdSetMode, kdGraphics); err != nil {
		logs.Printf(logs.GSPStopErr, "gsp: wall console %s graphics mode: %v", tty, err)
		f.Close()
		return
	}
	dev := wallFramebuffer()
	if err := blankFramebuffer(dev, filepath.Join("/sys/class/graphics", filepath.Base(dev))); err != nil {
		logs.Printf(logs.GSPStopErr, "gsp: blank framebuffer: %v", err)
	}
	return func() {
		unix.IoctlSetInt(int(f.Fd()), kdSetMode, kdText)
		f.Close()
	}
}

func wallFramebuffer() string {
	if dev := os.Getenv("CUTEPI_FB"); dev != "" {
		return dev
	}
	return "/dev/fb0"
}
