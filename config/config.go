package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type Config struct {
	Port           int    `json:"port"`
	Loop           bool   `json:"loop"`
	AuthPassword   string `json:"auth_password,omitempty"` // empty = no auth (LAN default)
	WorkingDir     string `json:"working_dir"`
	ConfigFilePath string `json:"config_file_path"`
	Db         struct {
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
	defaultPort         = 3000
	defaultWorkingDir   = "cutepi"
	defaultConfigDir    = "config"
	defaultConfig       = "config.json"
	defaultDb           = "ctp.db"
	defaultMediaDir     = "media"
	defaultThumbsDir    = "thumbnails"
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
// and a default config file on first run if they don't already exist.
func LoadConfig() {
	if err := ensureDirs(); err != nil {
		println("Error creating CuTePi directories:", err.Error())
	}

	confMu.Lock()
	file, err := os.Open(conf.ConfigFilePath)
	if err == nil {
		if derr := json.NewDecoder(file).Decode(&conf); derr != nil {
			println("Error reading config file:", derr.Error())
		}
		file.Close()
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
	// the lock itself).
	if err != nil {
		SaveConfig()
	}
}

// Save configuration to the config file. Atomic: encode to a temp sidecar
// and rename over the real file, so a crash or power-cut mid-write cannot
// leave a truncated config.json (which LoadConfig would silently replace
// with defaults, losing the operator's port/auth settings). Snapshot and
// encode under RLock so a concurrent setter can't partially update fields
// mid-write; the temp+rename stays safe under concurrent saves (unique temp
// per call, last rename wins).
func SaveConfig() {
	confMu.RLock()
	defer confMu.RUnlock()

	if err := ensureDirsLocked(); err != nil {
		println("Error creating CuTePi directories:", err.Error())
		return
	}

	// Unique temp sidecar per call (os.CreateTemp): concurrent SaveConfigs
	// (a settings POST can refire several setters) each write their own file,
	// and the last atomic rename wins instead of two writers clobbering a
	// single shared ".tmp".
	file, err := os.CreateTemp(filepath.Dir(conf.ConfigFilePath), "config-*.tmp")
	if err != nil {
		println("Error creating config file:", err.Error())
		return
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
		return
	}
	if err := os.Rename(tmpPath, conf.ConfigFilePath); err != nil {
		os.Remove(tmpPath)
		println("Error replacing config file:", err.Error())
	}
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
func SetLoop(loop bool) {
	confMu.Lock()
	conf.Loop = loop
	confMu.Unlock()
	SaveConfig()
}

// SetPort validates and updates the configured port, persisting it.
// Takes effect only after a restart.
func SetPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	confMu.Lock()
	conf.Port = port
	confMu.Unlock()
	SaveConfig()
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
func SetAuthPassword(pw string) {
	confMu.Lock()
	conf.AuthPassword = strings.TrimSpace(pw)
	confMu.Unlock()
	SaveConfig()
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
	confMu.Lock()
	conf.Display = struct {
		Resolution string `json:"resolution"`
		Refresh    int    `json:"refresh_hz"`
		UseEDID    bool   `json:"use_edid"`
	}{resolution, refreshHz, useEDID}
	confMu.Unlock()
	SaveConfig()
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
	if rate < 0 || rate > 192000 {
		return fmt.Errorf("invalid sample rate %d", rate)
	}
	switch channels {
	case "", "2.0", "2.1", "5.1", "7.1":
	default:
		return fmt.Errorf("invalid channel layout %q", channels)
	}
	confMu.Lock()
	conf.Audio.Device, conf.Audio.Channels, conf.Audio.Rate = strings.TrimSpace(device), channels, rate
	confMu.Unlock()
	SaveConfig()
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
	if enabled {
		if len(ssid) < 1 || len(ssid) > 32 {
			return fmt.Errorf("SSID must be 1-32 characters")
		}
		if len(pass) != 0 && len(pass) < 8 {
			return fmt.Errorf("WPA password must be at least 8 characters (or empty for open)")
		}
	}
	confMu.Lock()
	conf.AP = struct {
		Enabled  bool   `json:"enabled"`
		SSID     string `json:"ssid"`
		Password string `json:"password"`
	}{enabled, ssid, pass}
	confMu.Unlock()
	SaveConfig()
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
