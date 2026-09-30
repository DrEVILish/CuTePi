package gsp

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"CuTePi/logs"
)

// fbdevsink paints into the fbdev framebuffer and leaves the last frame
// there when its pipeline is torn down, so Stop/Panic/ESC/end-of-clip would
// leave a frozen picture on the wall instead of black (§6.9). blankWall
// clears the framebuffer explicitly. No-op for every other wall sink.
const eosBlankDelay = 150 * time.Millisecond

func blankWall(delay time.Duration) {
	if wallVideoSink() != "fbdevsink" {
		return
	}
	mgr.mu.Lock()
	gen := mgr.gen
	mgr.mu.Unlock()
	run := func() {
		mgr.mu.Lock()
		superseded := mgr.gen != gen
		mgr.mu.Unlock()
		if superseded {
			return
		}
		dev := wallFramebuffer()
		if err := blankFramebuffer(dev, filepath.Join("/sys/class/graphics", filepath.Base(dev))); err != nil {
			logs.Printf(logs.GSPStopErr, "gsp: blank framebuffer: %v", err)
		}
	}
	if delay <= 0 {
		run()
		return
	}
	time.AfterFunc(delay, run)
}

// blankFramebuffer zero-fills dev; its size is stride x height from sysDir.
func blankFramebuffer(dev, sysDir string) error {
	readInt := func(name string, idx int) (int, error) {
		b, err := os.ReadFile(filepath.Join(sysDir, name))
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.Split(strings.TrimSpace(string(b)), ",")[idx])
	}
	stride, err := readInt("stride", 0)
	if err != nil {
		return err
	}
	height, err := readInt("virtual_size", 1)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	zeros := make([]byte, stride*64)
	for written, total := 0, stride*height; written < total; {
		n := len(zeros)
		if total-written < n {
			n = total - written
		}
		if _, err := f.Write(zeros[:n]); err != nil {
			return err
		}
		written += n
	}
	return nil
}
