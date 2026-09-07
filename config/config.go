package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Port           int    `json:"port"`
	PollInterval   int    `json:"poll_interval_ms"`
	Loop           bool   `json:"loop"`
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
}

var conf Config

const (
	defaultPort         = 3000
	defaultPollInterval = 100
	minPollInterval     = 10
	defaultWorkingDir   = "CTP"
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
	c.PollInterval, _ = strconv.Atoi(getenv("POLL_INTERVAL_MS"))
	if c.PollInterval == 0 {
		c.PollInterval = defaultPollInterval
	}
	return c
}

func init() {
	// os.UserHomeDir: $HOME on unix, USERPROFILE on Windows. An error means
	// no home is set - resolveDefaults handles the empty string (paths then
	// come from the env overrides / working dir).
	homePath, err := os.UserHomeDir()
	if err != nil {
		homePath = ""
	}
	conf = resolveDefaults(os.Getenv, homePath)
}

// ensureDirs creates the working, config, media, and thumbnail directories
// (and the config file's and db file's parent directories) if they are missing,
// so first-run on a clean system doesn't panic on file creation.
func ensureDirs() error {
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

	// Read config file if it exists
	file, err := os.Open(conf.ConfigFilePath)
	if err == nil {
		defer file.Close()
		decoder := json.NewDecoder(file)
		err = decoder.Decode(&conf)
		if err != nil {
			println("Error reading config file:", err.Error())
		}
	} else {
		// Create the config file with default values if it doesn't exist
		SaveConfig()
	}

	// Environment overrides are intentional launch-time settings. Reapply them
	// after loading the file so PORT/POLL_INTERVAL_MS cannot be silently
	// replaced by stale persisted values.
	if raw := os.Getenv("PORT"); raw != "" {
		if port, parseErr := strconv.Atoi(raw); parseErr == nil && port > 0 {
			conf.Port = port
		}
	}
	if raw := os.Getenv("POLL_INTERVAL_MS"); raw != "" {
		if interval, parseErr := strconv.Atoi(raw); parseErr == nil && interval > 0 {
			conf.PollInterval = interval
		}
	}

	if conf.PollInterval < minPollInterval {
		conf.PollInterval = minPollInterval
	}
}

// Save configuration to the config file. Atomic: encode to a temp sidecar
// and rename over the real file, so a crash or power-cut mid-write cannot
// leave a truncated config.json (which LoadConfig would silently replace
// with defaults, losing the operator's port/auth settings).
func SaveConfig() {
	if err := ensureDirs(); err != nil {
		println("Error creating CuTePi directories:", err.Error())
		return
	}

	tmpPath := conf.ConfigFilePath + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		println("Error creating config file:", err.Error())
		return
	}
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
	return conf.Port
}

// PollInterval returns the client poll interval, in milliseconds.
func PollInterval() int {
	return conf.PollInterval
}

// Loop returns whether newly loaded clips loop back to the start on end-of-
// stream by default.
func Loop() bool {
	return conf.Loop
}

// SetLoop updates and persists the default loop-on-end behaviour.
func SetLoop(loop bool) {
	conf.Loop = loop
	SaveConfig()
}

// SetPollInterval validates and updates the poll interval, persisting it.
func SetPollInterval(ms int) error {
	if ms < minPollInterval {
		return fmt.Errorf("poll interval must be >= %dms", minPollInterval)
	}
	conf.PollInterval = ms
	SaveConfig()
	return nil
}

// SetPort validates and updates the configured port, persisting it.
// Takes effect only after a restart.
func SetPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	conf.Port = port
	SaveConfig()
	return nil
}

// AuthPassword returns the operator password; empty means auth is disabled
// (the default for a trusted show LAN).
func AuthPassword() string {
	return conf.AuthPassword
}

// HasAuth reports whether the operator password is enabled.
func HasAuth() bool {
	return conf.AuthPassword != ""
}

// SetAuthPassword sets (non-empty) or clears (empty) the operator password,
// persisting it. Applies to new requests immediately.
func SetAuthPassword(pw string) {
	conf.AuthPassword = strings.TrimSpace(pw)
	SaveConfig()
}

// WorkingDir returns the working directory from the configuration
func WorkingDir() string {
	return conf.WorkingDir
}

// DbLocation returns the database location from the configuration
func DbLocation() string {
	return conf.Db.Location
}

// SetDbLocation overrides the DB location in memory without persisting it.
// Intended for tests that want an isolated (e.g. in-memory sqlite) database.
func SetDbLocation(path string) {
	conf.Db.Location = path
}

// SetConfigFilePath overrides where SaveConfig/LoadConfig read and write,
// so tests don't touch the real user config file.
func SetConfigFilePath(path string) {
	conf.ConfigFilePath = path
}

// SetDirsForTesting overrides all working directories (working dir, media,
// thumbnails) so SaveConfig's ensureDirs doesn't touch the real user
// filesystem during tests.
func SetDirsForTesting(dir string) {
	conf.WorkingDir = dir
	conf.Media.Location = dir
	conf.Thumbnails.Location = dir
}

// MediaLocation returns the media location from the configuration
func MediaLocation() string {
	return conf.Media.Location
}

// ThumbnailLocation returns the thumbnail storage location from the configuration
func ThumbnailLocation() string {
	return conf.Thumbnails.Location
}
