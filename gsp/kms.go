package gsp

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// KMS wall (§6.1): every playing cue gets its own hardware display plane
// (kmssink on a shared DRM file descriptor). The display controller blends
// the planes itself — per-plane alpha, stacking order (zpos), position/size
// and 0°/180°/mirror rotation — so fades, crossfades, opacity and geometry
// cost no CPU and run at the display's own refresh rate. The primary plane
// underneath holds the (black) console framebuffer.

const (
	drmObjectPlane = 0xeeeeeeee
	drmObjectCRTC  = 0xcccccccc

	drmCapUniversalPlanes = 2
	drmCapAtomic          = 3

	drmPlaneTypeOverlay = 0

	// DRM "rotation" property bits.
	rotate0   = 1 << 0
	rotate180 = 1 << 2
	reflectX  = 1 << 4
	reflectY  = 1 << 5

	// DRM "pixel blend mode" values (drm_blend.h). GStreamer frames carry
	// straight (non-premultiplied) alpha, which is Coverage; the kernel's
	// default, Pre-multiplied, brightens every semi-transparent pixel.
	blendCoverage = 1
)

// ioctl request numbers (drm.h / drm_mode.h, 'd' = 0x64, _IOWR = 0xC0...).
const (
	ioctlSetClientCap        = 0x4010640d
	ioctlSetMaster           = 0x641e
	ioctlModeGetResources    = 0xC04064A0
	ioctlModeGetCRTC         = 0xC06864A1
	ioctlModeGetProperty     = 0xC04064AA
	ioctlModeGetPlaneRes     = 0xC01064B5
	ioctlModeGetPlane        = 0xC02064B6
	ioctlModeObjGetProps     = 0xC02064B9
	ioctlModeObjSetProperty  = 0xC01864BA
	drmPropNameLen           = 32
	drmDisplayModeNameLength = 32
)

type drmSetClientCap struct{ Capability, Value uint64 }

type drmModeCardRes struct {
	FbIDPtr, CrtcIDPtr, ConnectorIDPtr, EncoderIDPtr uint64
	CountFbs, CountCrtcs, CountConnectors, CountEnc  uint32
	MinWidth, MaxWidth, MinHeight, MaxHeight         uint32
}

type drmModeInfo struct {
	Clock                                         uint32
	Hdisplay, HsyncStart, HsyncEnd, Htotal, Hskew uint16
	Vdisplay, VsyncStart, VsyncEnd, Vtotal, Vscan uint16
	Vrefresh, Flags, Type                         uint32
	Name                                          [drmDisplayModeNameLength]byte
}

type drmModeCRTC struct {
	SetConnectorsPtr uint64
	CountConnectors  uint32
	CrtcID, FbID     uint32
	X, Y             uint32
	GammaSize        uint32
	ModeValid        uint32
	Mode             drmModeInfo
}

type drmModeGetPlaneRes struct {
	PlaneIDPtr  uint64
	CountPlanes uint32
	_           uint32
}

type drmModeGetPlane struct {
	PlaneID, CrtcID, FbID, PossibleCrtcs, GammaSize, CountFormatTypes uint32
	FormatTypePtr                                                     uint64
}

type drmModeObjGetProps struct {
	PropsPtr, PropValuesPtr uint64
	CountProps, ObjID       uint32
	ObjType, _              uint32
}

type drmModeGetProperty struct {
	ValuesPtr, EnumBlobPtr uint64
	PropID, Flags          uint32
	Name                   [drmPropNameLen]byte
	CountValues, CountEnum uint32
}

type drmModeObjSetProperty struct {
	Value                  uint64
	PropID, ObjID, ObjType uint32
	_                      uint32
}

func drmIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		switch e {
		case 0:
			return nil
		case unix.EINTR, unix.EAGAIN:
			continue
		default:
			return e
		}
	}
}

// kmsPlane is one overlay plane usable on the wall's CRTC.
type kmsPlane struct {
	id    uint32
	props map[string]uint32 // property name -> id
}

// KMSWall is the open display: one DRM fd shared by every kmssink, the CRTC
// feeding HDMI, its current mode, and the overlay planes it can use.
type KMSWall struct {
	fd      int
	crtcID  uint32
	Width   int
	Height  int
	Refresh int // Hz, from the active display mode

	mu     sync.Mutex
	planes []*kmsPlane
	busy   map[uint32]bool
}

// openKMSWall finds the DRM device driving a display (the CRTC with an
// active mode) and the overlay planes it accepts.
func openKMSWall() (*KMSWall, error) {
	var lastErr error = errors.New("no DRM device with an active display")
	for i := 0; i < 4; i++ {
		path := fmt.Sprintf("/dev/dri/card%d", i)
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			if !os.IsNotExist(err) {
				lastErr = err
			}
			continue
		}
		w, err := probeKMSWall(fd)
		if err != nil {
			unix.Close(fd)
			lastErr = fmt.Errorf("%s: %w", path, err)
			continue
		}
		return w, nil
	}
	return nil, lastErr
}

func probeKMSWall(fd int) (*KMSWall, error) {
	// Plane properties can only be written by the DRM master. Claim it
	// explicitly: whoever opens the device first gets it implicitly, and a
	// diagnostic tool opened before us would otherwise hold it.
	if err := drmIoctl(fd, ioctlSetMaster, nil); err != nil {
		return nil, fmt.Errorf("not the display master (another program owns the display): %w", err)
	}
	for _, c := range []drmSetClientCap{{drmCapUniversalPlanes, 1}, {drmCapAtomic, 1}} {
		if err := drmIoctl(fd, ioctlSetClientCap, unsafe.Pointer(&c)); err != nil {
			return nil, fmt.Errorf("client cap %d: %w", c.Capability, err)
		}
	}
	var res drmModeCardRes
	if err := drmIoctl(fd, ioctlModeGetResources, unsafe.Pointer(&res)); err != nil {
		return nil, err
	}
	if res.CountCrtcs == 0 {
		return nil, errors.New("no CRTCs")
	}
	crtcs := make([]uint32, res.CountCrtcs)
	res2 := drmModeCardRes{CrtcIDPtr: uint64(uintptr(unsafe.Pointer(&crtcs[0]))), CountCrtcs: res.CountCrtcs}
	if err := drmIoctl(fd, ioctlModeGetResources, unsafe.Pointer(&res2)); err != nil {
		return nil, err
	}
	w := &KMSWall{fd: fd, busy: map[uint32]bool{}}
	crtcIndex := -1
	for i, id := range crtcs {
		c := drmModeCRTC{CrtcID: id}
		if err := drmIoctl(fd, ioctlModeGetCRTC, unsafe.Pointer(&c)); err != nil {
			continue
		}
		if c.ModeValid != 0 && c.Mode.Hdisplay > 0 {
			w.crtcID, crtcIndex = id, i
			w.Width, w.Height, w.Refresh = int(c.Mode.Hdisplay), int(c.Mode.Vdisplay), int(c.Mode.Vrefresh)
			break
		}
	}
	if crtcIndex < 0 {
		return nil, errors.New("no active display mode")
	}
	var pr drmModeGetPlaneRes
	if err := drmIoctl(fd, ioctlModeGetPlaneRes, unsafe.Pointer(&pr)); err != nil {
		return nil, err
	}
	if pr.CountPlanes == 0 {
		return nil, errors.New("no planes")
	}
	ids := make([]uint32, pr.CountPlanes)
	pr2 := drmModeGetPlaneRes{PlaneIDPtr: uint64(uintptr(unsafe.Pointer(&ids[0]))), CountPlanes: pr.CountPlanes}
	if err := drmIoctl(fd, ioctlModeGetPlaneRes, unsafe.Pointer(&pr2)); err != nil {
		return nil, err
	}
	type cand struct {
		p    *kmsPlane
		zpos uint64
	}
	var cands []cand
	for _, id := range ids {
		gp := drmModeGetPlane{PlaneID: id}
		if err := drmIoctl(fd, ioctlModeGetPlane, unsafe.Pointer(&gp)); err != nil {
			continue
		}
		if gp.PossibleCrtcs&(1<<crtcIndex) == 0 {
			continue
		}
		props, values, err := objectProps(fd, id, drmObjectPlane)
		if err != nil {
			continue
		}
		if values["type"] != drmPlaneTypeOverlay {
			continue
		}
		if _, ok := props["alpha"]; !ok {
			continue
		}
		if _, ok := props["zpos"]; !ok {
			continue
		}
		cands = append(cands, cand{&kmsPlane{id: id, props: props}, values["zpos"]})
	}
	if len(cands) == 0 {
		return nil, errors.New("no overlay planes with alpha and zpos")
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].zpos < cands[j].zpos })
	for _, c := range cands {
		w.planes = append(w.planes, c.p)
	}
	return w, nil
}

// objectProps reads an object's property ids and current values by name.
func objectProps(fd int, obj uint32, objType uint32) (map[string]uint32, map[string]uint64, error) {
	q := drmModeObjGetProps{ObjID: obj, ObjType: objType}
	if err := drmIoctl(fd, ioctlModeObjGetProps, unsafe.Pointer(&q)); err != nil {
		return nil, nil, err
	}
	if q.CountProps == 0 {
		return map[string]uint32{}, map[string]uint64{}, nil
	}
	pids := make([]uint32, q.CountProps)
	vals := make([]uint64, q.CountProps)
	q.PropsPtr = uint64(uintptr(unsafe.Pointer(&pids[0])))
	q.PropValuesPtr = uint64(uintptr(unsafe.Pointer(&vals[0])))
	if err := drmIoctl(fd, ioctlModeObjGetProps, unsafe.Pointer(&q)); err != nil {
		return nil, nil, err
	}
	ids := map[string]uint32{}
	values := map[string]uint64{}
	for i, pid := range pids[:q.CountProps] {
		gp := drmModeGetProperty{PropID: pid}
		if err := drmIoctl(fd, ioctlModeGetProperty, unsafe.Pointer(&gp)); err != nil {
			continue
		}
		name := string(gp.Name[:])
		for j, b := range gp.Name {
			if b == 0 {
				name = string(gp.Name[:j])
				break
			}
		}
		ids[name] = pid
		values[name] = vals[i]
	}
	return ids, values, nil
}

// acquire hands out a free overlay plane (nil when all are in use).
func (w *KMSWall) acquire() *kmsPlane {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.planes {
		if !w.busy[p.id] {
			w.busy[p.id] = true
			return p
		}
	}
	return nil
}

func (w *KMSWall) release(p *kmsPlane) {
	if p == nil {
		return
	}
	w.mu.Lock()
	delete(w.busy, p.id)
	w.mu.Unlock()
}

// set writes one plane property; unknown properties are ignored.
func (w *KMSWall) set(p *kmsPlane, name string, value uint64) error {
	pid, ok := p.props[name]
	if !ok {
		return nil
	}
	s := drmModeObjSetProperty{Value: value, PropID: pid, ObjID: p.id, ObjType: drmObjectPlane}
	return drmIoctl(w.fd, ioctlModeObjSetProperty, unsafe.Pointer(&s))
}

// framePeriod is one refresh of the active display mode.
func (w *KMSWall) framePeriod() time.Duration {
	hz := w.Refresh
	if hz <= 0 {
		hz = 60
	}
	return time.Second / time.Duration(hz)
}

// setAlpha maps 0..1 to the plane's 16-bit alpha.
func (w *KMSWall) setAlpha(p *kmsPlane, a float64) error {
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	return w.set(p, "alpha", uint64(a*65535+0.5))
}

// planeState reads back a plane's properties (tests and diagnostics).
func (w *KMSWall) planeState(p *kmsPlane) map[string]uint64 {
	_, v, _ := objectProps(w.fd, p.id, drmObjectPlane)
	return v
}

// rotationBits maps cue rotation/mirror onto the plane's rotation property.
// ok=false for 90/270, which the display controller cannot rotate.
func rotationBits(deg int, flip string) (bits uint64, ok bool) {
	switch deg {
	case 0:
		bits = rotate0
	case 180:
		bits = rotate180
	default:
		return 0, false
	}
	switch flip {
	case "h":
		bits |= reflectX
	case "v":
		bits |= reflectY
	}
	return bits, true
}
