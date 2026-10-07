package gsp

import (
	"errors"
	"fmt"
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

// wallTTY is the virtual terminal sharing the HDMI output: CUTEPI_TTY, or
// the active one.
func wallTTY() (string, error) {
	if tty := os.Getenv("CUTEPI_TTY"); tty != "" {
		return tty, nil
	}
	active, err := os.ReadFile("/sys/class/tty/tty0/active")
	if err != nil {
		return "", err
	}
	return "/dev/" + strings.TrimSpace(string(active)), nil
}

// SetConsoleMode switches the wall's virtual terminal to graphics (the
// kernel stops drawing on it) or back to text. The mode stays after the
// caller exits, which is what lets a root step in the systemd unit set it
// for a service that runs as an unprivileged user (KDSETMODE needs
// CAP_SYS_TTY_CONFIG, and getty keeps the tty owner-only). Write-only open:
// the ioctl needs no read access.
func SetConsoleMode(graphics bool) error {
	tty, err := wallTTY()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(tty, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	mode := kdText
	if graphics {
		mode = kdGraphics
	}
	if err := unix.IoctlSetInt(int(f.Fd()), kdSetMode, mode); err != nil {
		return fmt.Errorf("%s: %w", tty, err)
	}
	return nil
}

// ClaimWallConsole switches the virtual terminal sharing the HDMI output to
// graphics mode: the kernel then stops drawing anything on it — login prompt,
// kernel messages, the blinking cursor — so the wall shows only what CuTePi
// puts there (§6.9 "Black means black"). The framebuffer is cleared to black.
// The returned func restores text mode for a clean shutdown.
//
// Running as the unprivileged service user, the tty is out of reach: the
// unit's root ExecStartPre (cutepi --console graphics) has set the mode
// already and ExecStopPost restores it, so that is only noted at debug
// level, and the framebuffer (video group) is still blanked.
func ClaimWallConsole() (release func()) {
	release = func() {}
	if err := SetConsoleMode(true); err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EPERM) {
			// Expected under cutepi.service: its root ExecStartPre set it.
			logs.PrintfDebug(logs.GSPStopErr, "gsp: wall console graphics mode is set by the service unit (running unprivileged)")
		} else {
			logs.Printf(logs.GSPStopErr, "gsp: wall console graphics mode: %v", err)
		}
	} else {
		release = func() {
			if err := SetConsoleMode(false); err != nil {
				logs.Printf(logs.GSPStopErr, "gsp: wall console text mode: %v", err)
			}
		}
	}
	dev := wallFramebuffer()
	if err := blankFramebuffer(dev, filepath.Join("/sys/class/graphics", filepath.Base(dev))); err != nil {
		logs.Printf(logs.GSPStopErr, "gsp: blank framebuffer: %v", err)
	}
	return release
}

func wallFramebuffer() string {
	if dev := os.Getenv("CUTEPI_FB"); dev != "" {
		return dev
	}
	return "/dev/fb0"
}
