package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Port int  `json:"port"`
	Loop bool `json:"loop"`
	// AuthPassword is the optional operator password (HTTP Basic). It is
	// stored in PLAIN TEXT in config.json (file mode 0600): anyone who can
	// read the data directory can read it. See DESIGN.md "Operator password".
	AuthPassword   string `json:"auth_password,omitempty"` // empty = no auth (LAN default)
	WorkingDir     string `json:"working_dir"`
	ConfigFilePath string `json:"config_file_path"`
	Db             struct {
		Location string `json:"location"`
	} `json:"db"`
	Media struct {
		Location string `json:"location"`
	} `json:"media"`
	Thumbnails struct {
		Location string `json:"location"`
	} `json:"thumbnails"`
	// Display/Audio tune the pipeline (GStreamer caps + sink choice);
	// UseEDID hands mode control to the connected destination instead.
	Display struct {
		Resolution string `json:"resolution"` // "1920x1080"; "" = no forced caps
		Refresh    int    `json:"refresh_hz"` // wall refresh in Hz; 0 = unset
		UseEDID    bool   `json:"use_edid"`   // trust the destination's EDID over manual numbers
	} `json:"display"`
	Audio struct {
		Device   string `json:"device"`   // ALSA/Pulse sink name; "" = automatic
		Channels string `json:"channels"` // "2.0" default
		Rate     int    `json:"rate"`     // sink sample rate; 0 = pipe as-is
	} `json:"audio"`
	AP struct {
		Enabled  bool   `json:"enabled"`
		SSID     string `json:"ssid"`
		Password string `json:"password"`
	} `json:"hotspot"`
	Remote RemoteSettings `json:"remote"`
}

// RemoteSettings are the remote-control listeners (§12.8). Neither protocol
// authenticates, so every listener is off by default; ports and the OSC bind
// address are operator-configurable from the Settings Network tab.
type RemoteSettings struct {
	HyperDeck      bool   `json:"hyperdeck"`
	HyperDeckPort  int    `json:"hyperdeck_port"`
	HyperDeckClips string `json:"hyperdeck_clips"` // "cuesheet" (default) | "mediapool"
	OSCUDP         bool   `json:"osc_udp"`
	OSCUDPPort     int    `json:"osc_udp_port"`
	OSCTCP         bool   `json:"osc_tcp"`
	OSCTCPPort     int    `json:"osc_tcp_port"`
	OSCBind        string `json:"osc_bind"` // "" = all interfaces
}

const (
	DefaultHyperDeckPort = 9993
	DefaultOSCPort       = 53000
)

// withRemoteDefaults fills unset ports / clip source with the protocol
// defaults (an old config file pre-dating the Network tab has zeros).
func withRemoteDefaults(r RemoteSettings) RemoteSettings {
	if r.HyperDeckPort == 0 {
		r.HyperDeckPort = DefaultHyperDeckPort
	}
	if r.OSCUDPPort == 0 {
		r.OSCUDPPort = DefaultOSCPort
	}
	if r.OSCTCPPort == 0 {
		r.OSCTCPPort = DefaultOSCPort
	}
	if r.HyperDeckClips != "mediapool" {
		r.HyperDeckClips = "cuesheet"
	}
	return r
}

// Remote returns the remote-control listener settings, defaults applied.
func Remote() RemoteSettings {
	confMu.RLock()
	defer confMu.RUnlock()
	return withRemoteDefaults(conf.Remote)
}

// SetRemote validates and persists the remote-control listener settings
// (persist ONLY; starting/stopping listeners is the routes' job).
func SetRemote(r RemoteSettings) error {
	r, err := ValidateRemote(r)
	if err != nil {
		return err
	}
	confMu.Lock()
	conf.Remote = r
	confMu.Unlock()
	return SaveConfig()
}

// ValidateRemote normalises r (defaults applied, bind trimmed) and checks it
// without persisting, so a multi-field settings save can validate every
// field before writing any.
func ValidateRemote(r RemoteSettings) (RemoteSettings, error) {
	r.OSCBind = strings.TrimSpace(r.OSCBind)
	r = withRemoteDefaults(r)
	for name, p := range map[string]int{"HyperDeck": r.HyperDeckPort, "OSC UDP": r.OSCUDPPort, "OSC TCP": r.OSCTCPPort} {
		if p < 1 || p > 65535 {
			return r, fmt.Errorf("invalid %s port %d", name, p)
		}
	}
	if r.OSCBind != "" && net.ParseIP(r.OSCBind) == nil {
		return r, fmt.Errorf("invalid bind address %q (an IP address, or empty for all interfaces)", r.OSCBind)
	}
	return r, nil
}

// conf is read by the auth middleware and every settings getter on their own
// goroutine while settings POSTs (and the app handlers) mutate it, so all
// access rides an RWMutex. SaveConfig/LoadConfig take the lock themselves;
// setters mutate under Lock, then persist via SaveConfig.
var (
	conf   Config
	confMu sync.RWMutex
)

const (
	defaultPort       = 3001 // dev default; the systemd unit sets PORT=80
	defaultWorkingDir = "cutepi"
	defaultConfigDir  = "config"
	defaultConfig     = "config.json"
	defaultDb         = "ctp.db"
	defaultMediaDir   = "media"
	defaultThumbsDir  = "thumbnails"
)

// expandHome expands a leading "~" or "~/" in path to the user's home directory.
func expandHome(path, homePath string) string {
	if path == "~" {
		return homePath
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(homePath, path[2:])
	}
	return path
}

// resolveDefaults computes the default Config from environment variables
// and the user's home directory. It's a pure function (no package-level
// state) so the path-derivation rules - in particular that ConfigFilePath/
// Db/Media/Thumbnails all fall back to living under WorkingDir - can be
// unit tested directly, without relying on package init() order.
func resolveDefaults(getenv func(string) string, homePath string) Config {
	var c Config

	// WorkingDir must be resolved first: every other path below falls back
	// to living under it, so that setting WORKING_DIR alone relocates the
	// whole tree consistently.
	c.WorkingDir = expandHome(getenv("WORKING_DIR"), homePath)
	if c.WorkingDir == "" {
		c.WorkingDir = filepath.Join(homePath, defaultWorkingDir)
	}

	c.ConfigFilePath = expandHome(getenv("CONFIG_PATH"), homePath)
	if c.ConfigFilePath == "" {
		c.ConfigFilePath = filepath.Join(c.WorkingDir, defaultConfigDir, defaultConfig)
	}

	c.Db.Location = expandHome(getenv("DB_PATH"), homePath)
	if c.Db.Location == "" {
		c.Db.Location = filepath.Join(c.WorkingDir, defaultConfigDir, defaultDb)
	}
	c.Media.Location = expandHome(getenv("MEDIA_DIR"), homePath)
	if c.Media.Location == "" {
		c.Media.Location = filepath.Join(c.WorkingDir, defaultMediaDir)
	}
	c.Thumbnails.Location = expandHome(getenv("THUMBNAILS_DIR"), homePath)
	if c.Thumbnails.Location == "" {
		c.Thumbnails.Location = filepath.Join(c.WorkingDir, defaultThumbsDir)
	}
	c.Port, _ = strconv.Atoi(getenv("PORT"))
	if c.Port == 0 {
		c.Port = defaultPort
	}
	return c
}

func init() {
	// os.UserHomeDir reads $HOME, but a systemd service runs without it (and
	// an empty home silently turns "cutepi" into a relative path beside the
	// binary). Fall back to the invoking user's passwd entry.
	homePath, err := os.UserHomeDir()
	if err != nil || homePath == "" {
		if u, uerr := user.Current(); uerr == nil && u.HomeDir != "" {
			homePath = u.HomeDir
		} else {
			homePath = ""
		}
	}
	conf = resolveDefaults(os.Getenv, homePath)
}

// ensureDirs creates the working, config, media, and thumbnail directories
// (and the config file's and db file's parent directories) if they are missing,
// so first-run on a clean system doesn't panic on file creation.
func ensureDirs() error {
	confMu.RLock()
	defer confMu.RUnlock()
	return ensureDirsLocked()
}

// ensureDirsLocked is the lock-free body of ensureDirs; callers already
// holding confMu (SaveConfig snapshots under RLock) use it directly.
func ensureDirsLocked() error {
	dirs := []string{
		conf.WorkingDir,
		filepath.Dir(conf.ConfigFilePath),
		filepath.Dir(conf.Db.Location),
		conf.Media.Location,
		conf.Thumbnails.Location,
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// Load configuration from the config file. Creates the working directories
// and a default config file on first run (no file at all).
//
// A config file that exists but cannot be used is never half-applied: it is
// decoded into a copy of the defaults, which only replaces the running
// configuration if the whole file is valid JSON. A malformed file is
// recovered from config.json.last-good (written after every good load and
// save) and moved aside as config.json.broken-<time> for inspection; with no
// usable last-good copy, LoadConfig returns an error and the service does
// not start (the broken file stays in place, so a restart cannot mistake it
// for a first run and write defaults over the operator's settings).
func LoadConfig() error {
	if err := ensureDirs(); err != nil {
		println("Error creating CuTePi directories:", err.Error())
	}

	confMu.Lock()
	path := conf.ConfigFilePath
	raw, err := os.ReadFile(path)
	firstRun := errors.Is(err, fs.ErrNotExist)
	if err != nil && !firstRun {
		confMu.Unlock()
		return fmt.Errorf("reading config file %s: %w", path, err)
	}
	if !firstRun {
		next, derr := decodeConfig(conf, raw)
		if derr != nil {
			recovered, rerr := recoverConfig(conf, path, derr)
			if rerr != nil {
				confMu.Unlock()
				return rerr
			}
			next = recovered
		} else {
			writeLastGood(path, raw)
		}
		conf = next
	}

	// Environment overrides are intentional launch-time settings. Reapply them
	// after loading the file so PORT/POLL_INTERVAL_MS cannot be silently
	// replaced by stale persisted values.
	if raw := os.Getenv("PORT"); raw != "" {
		if port, parseErr := strconv.Atoi(raw); parseErr == nil && port > 0 {
			conf.Port = port
		}
	}
	// HDMI-2.0@48k defaults: a fresh config (or an old file pre-dating the
	// Audio tab) gets the projector default; explicit zeros stay valid.
	if conf.Audio.Channels == "" {
		conf.Audio.Channels = "2.0"
	}
	if conf.Audio.Rate == 0 {
		conf.Audio.Rate = 48000
	}
	confMu.Unlock()

	// First run: create the config file with default values (SaveConfig takes
	// the lock itself). A recovered config is written back the same way.
	if firstRun {
		_ = SaveConfig() // logged inside; first run keeps going on defaults
	}
	return nil
}

// decodeConfig decodes raw over a copy of base (fields the file leaves out
// keep base's values). Anything but exactly one JSON object is an error.
//
// The data paths (working dir, config file, DB, media, thumbnails) always
// stay base's: they come from the environment (WORKING_DIR, MEDIA_DIR, ...)
// or the defaults, never from the file. config.json still records them for
// reference, but a stored copy must not pin the data to an old location:
// moving the service to /var/lib/cutepi is then one WORKING_DIR setting.
func decodeConfig(base Config, raw []byte) (Config, error) {
	next := base
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&next); err != nil {
		return base, err
	}
	if dec.More() {
		return base, errors.New("unexpected data after the JSON object")
	}
	next.WorkingDir, next.ConfigFilePath = base.WorkingDir, base.ConfigFilePath
	next.Db.Location, next.Media.Location, next.Thumbnails.Location = base.Db.Location, base.Media.Location, base.Thumbnails.Location
	return next, nil
}

// lastGoodPath and the broken-file name sit beside the config file.
func lastGoodPath(path string) string { return path + ".last-good" }

// recoverConfig handles a malformed config file: the last good copy is
// decoded instead, the broken file is moved aside and the recovered config
// written back. Called with confMu held.
func recoverConfig(base Config, path string, cause error) (Config, error) {
	good, err := os.ReadFile(lastGoodPath(path))
	if err != nil {
		return base, fmt.Errorf("config file %s is malformed (%v) and there is no last good copy to recover from (%v): fix or remove it", path, cause, err)
	}
	next, err := decodeConfig(base, good)
	if err != nil {
		return base, fmt.Errorf("config file %s is malformed (%v) and its last good copy is too (%v): fix or remove it", path, cause, err)
	}
	broken := path + ".broken-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, broken); err != nil {
		return base, fmt.Errorf("config file %s is malformed (%v); moving it aside: %w", path, cause, err)
	}
	if err := writeFileAtomic(path, good); err != nil {
		return base, fmt.Errorf("restoring %s from its last good copy: %w", path, err)
	}
	log.Printf("CuTePi: config file %s was malformed (%v): restored the last good configuration; the broken file is %s", path, cause, broken)
	return next, nil
}

// writeLastGood keeps a copy of a config that loaded or saved fine.
func writeLastGood(path string, raw []byte) {
	if err := writeFileAtomic(lastGoodPath(path), raw); err != nil {
		log.Printf("CuTePi: saving last good config: %v", err)
	}
}

// writeFileAtomic writes data to path through a temp file and a rename.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Save configuration to the config file. Atomic: encode to a temp sidecar
// and rename over the real file, so a crash or power-cut mid-write cannot
// leave a truncated config.json (which LoadConfig would silently replace
// with defaults, losing the operator's port/auth settings). Snapshot and
// encode under RLock so a concurrent setter can't partially update fields
// mid-write; the temp+rename stays safe under concurrent saves (unique temp
// per call, last rename wins).
func SaveConfig() error {
	confMu.RLock()
	defer confMu.RUnlock()

	if err := ensureDirsLocked(); err != nil {
		println("Error creating CuTePi directories:", err.Error())
		return fmt.Errorf("creating config directories: %w", err)
	}

	// Unique temp sidecar per call (os.CreateTemp): concurrent SaveConfigs
	// (a settings POST can refire several setters) each write their own file,
	// and the last atomic rename wins instead of two writers clobbering a
	// single shared ".tmp".
	file, err := os.CreateTemp(filepath.Dir(conf.ConfigFilePath), "config-*.tmp")
	if err != nil {
		println("Error creating config file:", err.Error())
		return fmt.Errorf("creating config file: %w", err)
	}
	tmpPath := file.Name()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(conf)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmpPath)
		println("Error writing to config file:", err.Error())
		return fmt.Errorf("writing config file: %w", err)
	}
	if err := os.Rename(tmpPath, conf.ConfigFilePath); err != nil {
		os.Remove(tmpPath)
		println("Error replacing config file:", err.Error())
		return fmt.Errorf("replacing config file: %w", err)
	}
	if raw, err := os.ReadFile(conf.ConfigFilePath); err == nil {
		writeLastGood(conf.ConfigFilePath, raw)
	}
	return nil
}

// Port returns the port value from the configuration
func Port() int {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.Port
}

// Loop returns whether newly loaded clips loop back to the start on end-of-
// stream by default.
func Loop() bool {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.Loop
}

// SetLoop updates and persists the default loop-on-end behaviour.
func SetLoop(loop bool) error {
	confMu.Lock()
	conf.Loop = loop
	confMu.Unlock()
	return SaveConfig()
}

// SetPort validates and updates the configured port, persisting it.
// Takes effect only after a restart.
func SetPort(port int) error {
	if err := ValidatePort(port); err != nil {
		return err
	}
	confMu.Lock()
	conf.Port = port
	confMu.Unlock()
	return SaveConfig()
}

// ValidatePort checks a listen port without persisting it.
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

// AuthPassword returns the operator password; empty means auth is disabled
// (the default for a trusted show LAN).
func AuthPassword() string {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.AuthPassword
}

// HasAuth reports whether the operator password is enabled.
func HasAuth() bool {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.AuthPassword != ""
}

// SetAuthPassword sets (non-empty) or clears (empty) the operator password,
// persisting it. Applies to new requests immediately.
//
// The password is kept in plain text in config.json (see Config.AuthPassword).
// ErrPasswordSpaces: a password with leading or trailing whitespace is
// refused rather than silently trimmed (trimming made the password the
// operator typed fail at the login prompt).
var ErrPasswordSpaces = errors.New("the password can't start or end with a space")

func SetAuthPassword(pw string) error {
	if pw != strings.TrimSpace(pw) {
		return ErrPasswordSpaces
	}
	confMu.Lock()
	conf.AuthPassword = pw
	confMu.Unlock()
	return SaveConfig()
}

// TmpDir is the scratch directory for uploads, show imports and downloads:
// <working dir>/tmp. It lives on the data disk, not /tmp (a RAM-backed
// tmpfs on current Raspberry Pi OS), and normally shares a filesystem with
// the media directory so finished files move into place by rename.
func TmpDir() string {
	confMu.RLock()
	defer confMu.RUnlock()
	return filepath.Join(conf.WorkingDir, "tmp")
}

// Display returns the wall/output display settings.
func Display() struct {
	Resolution string
	RefreshHz  int
	UseEDID    bool
} {
	confMu.RLock()
	defer confMu.RUnlock()
	return struct {
		Resolution string
		RefreshHz  int
		UseEDID    bool
	}{conf.Display.Resolution, conf.Display.Refresh, conf.Display.UseEDID}
}

// SetDisplay validates and persists the display settings (manual mode).
func SetDisplay(resolution string, refreshHz int, useEDID bool) error {
	if err := ValidateDisplay(resolution, refreshHz); err != nil {
		return err
	}
	confMu.Lock()
	conf.Display = struct {
		Resolution string `json:"resolution"`
		Refresh    int    `json:"refresh_hz"`
		UseEDID    bool   `json:"use_edid"`
	}{resolution, refreshHz, useEDID}
	confMu.Unlock()
	return SaveConfig()
}

// ValidateDisplay checks display settings without persisting them.
func ValidateDisplay(resolution string, refreshHz int) error {
	if resolution != "" {
		parts := strings.SplitN(resolution, "x", 2)
		w, herr := strconv.Atoi(parts[0])
		var h int
		if len(parts) == 2 {
			h, herr = strconv.Atoi(parts[1])
		}
		if herr != nil || w < 320 || h < 240 || w > 7680 || h > 4320 {
			return fmt.Errorf("invalid resolution %q (want e.g. 1920x1080)", resolution)
		}
	}
	if refreshHz < 0 || refreshHz > 240 {
		return fmt.Errorf("invalid refresh rate %d", refreshHz)
	}
	return nil
}

// Audio returns the audio output settings.
func Audio() struct {
	Device   string
	Channels string
	Rate     int
} {
	confMu.RLock()
	defer confMu.RUnlock()
	return struct {
		Device   string
		Channels string
		Rate     int
	}{conf.Audio.Device, conf.Audio.Channels, conf.Audio.Rate}
}

// SetAudio validates and persists the audio output settings.
func SetAudio(device, channels string, rate int) error {
	if err := ValidateAudio(channels, rate); err != nil {
		return err
	}
	confMu.Lock()
	conf.Audio.Device, conf.Audio.Channels, conf.Audio.Rate = strings.TrimSpace(device), channels, rate
	confMu.Unlock()
	return SaveConfig()
}

// ValidateAudio checks audio settings without persisting them.
func ValidateAudio(channels string, rate int) error {
	if rate < 0 || rate > 192000 {
		return fmt.Errorf("invalid sample rate %d", rate)
	}
	switch channels {
	case "", "2.0", "2.1", "5.1", "7.1":
	default:
		return fmt.Errorf("invalid channel layout %q", channels)
	}
	return nil
}

// AP returns the Wi-Fi access-point settings.
func AP() struct {
	Enabled bool
	SSID    string
	Pass    string
} {
	confMu.RLock()
	defer confMu.RUnlock()
	return struct {
		Enabled bool
		SSID    string
		Pass    string
	}{conf.AP.Enabled, conf.AP.SSID, conf.AP.Password}
}

// SetAP validates and persists the Wi-Fi access-point settings (persist ONLY;
// enabling/disabling the actual hotspot is the network routes' system action).
func SetAP(ssid, pass string, enabled bool) error {
	if err := ValidateAP(ssid, pass, enabled); err != nil {
		return err
	}
	confMu.Lock()
	conf.AP = struct {
		Enabled  bool   `json:"enabled"`
		SSID     string `json:"ssid"`
		Password string `json:"password"`
	}{enabled, ssid, pass}
	confMu.Unlock()
	return SaveConfig()
}

// ValidateAP checks hotspot settings without persisting them.
func ValidateAP(ssid, pass string, enabled bool) error {
	if enabled {
		if len(ssid) < 1 || len(ssid) > 32 {
			return fmt.Errorf("SSID must be 1-32 characters")
		}
		if len(pass) != 0 && (len(pass) < 8 || len(pass) > 63) {
			return fmt.Errorf("WPA password must be 8-63 characters (or empty for open)")
		}
	}
	return nil
}

// WorkingDir returns the working directory from the configuration
func WorkingDir() string {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.WorkingDir
}

// DbLocation returns the database location from the configuration
func DbLocation() string {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.Db.Location
}

// SetDbLocation overrides the DB location in memory without persisting it.
// Intended for tests that want an isolated (e.g. in-memory sqlite) database.
func SetDbLocation(path string) {
	confMu.Lock()
	defer confMu.Unlock()
	conf.Db.Location = path
}

// SetConfigFilePath overrides where SaveConfig/LoadConfig read and write,
// so tests don't touch the real user config file.
func SetConfigFilePath(path string) {
	confMu.Lock()
	defer confMu.Unlock()
	conf.ConfigFilePath = path
}

// SetDirsForTesting overrides all working directories (working dir, media,
// thumbnails) so SaveConfig's ensureDirs doesn't touch the real user
// filesystem during tests.
func SetDirsForTesting(dir string) {
	confMu.Lock()
	defer confMu.Unlock()
	conf.WorkingDir = dir
	conf.Media.Location = dir
	conf.Thumbnails.Location = dir
}

// MediaLocation returns the media location from the configuration
func MediaLocation() string {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.Media.Location
}

// ThumbnailLocation returns the thumbnail storage location from the configuration
func ThumbnailLocation() string {
	confMu.RLock()
	defer confMu.RUnlock()
	return conf.Thumbnails.Location
}
