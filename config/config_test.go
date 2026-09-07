package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

var testDir string

func useTestDir(dir string) {
	SetConfigFilePath(filepath.Join(dir, "config.json"))
	SetDirsForTesting(dir)
	SetDbLocation(filepath.Join(dir, "ctp.db"))
}

func TestMain(m *testing.M) {
	// Redirect config persistence to a temp dir for the whole test binary,
	// so SetPort/SetPollInterval below don't write to the real user config.
	dir, err := os.MkdirTemp("", "cutepi-config-test")
	if err != nil {
		panic(err)
	}
	testDir = dir
	useTestDir(testDir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestExpandHome(t *testing.T) {
	home := "/home/pi"
	cases := map[string]string{
		"~":            "/home/pi",
		"~/CTP":        "/home/pi/CTP",
		"/abs/path":    "/abs/path",
		"":             "",
		"relative/dir": "relative/dir",
	}
	for in, want := range cases {
		if got := expandHome(in, home); got != want {
			t.Errorf("expandHome(%q, %q) = %q, want %q", in, home, got, want)
		}
	}
}

func TestSetPollIntervalValidation(t *testing.T) {
	if err := SetPollInterval(5); err == nil {
		t.Fatalf("expected error for poll interval below minimum")
	}
	if err := SetPollInterval(50); err != nil {
		t.Fatalf("SetPollInterval(50): %v", err)
	}
	if PollInterval() != 50 {
		t.Fatalf("expected PollInterval()=50, got %d", PollInterval())
	}
}

func TestSetPortValidation(t *testing.T) {
	if err := SetPort(0); err == nil {
		t.Fatalf("expected error for invalid port")
	}
	if err := SetPort(70000); err == nil {
		t.Fatalf("expected error for out-of-range port")
	}
	if err := SetPort(8080); err != nil {
		t.Fatalf("SetPort(8080): %v", err)
	}
	if Port() != 8080 {
		t.Fatalf("expected Port()=8080, got %d", Port())
	}
}

func TestSetLoopPersists(t *testing.T) {
	SetLoop(true)
	if !Loop() {
		t.Fatalf("expected Loop()=true after SetLoop(true)")
	}
	SetLoop(false)
	if Loop() {
		t.Fatalf("expected Loop()=false after SetLoop(false)")
	}
}

func fakeEnv(vars map[string]string) func(string) string {
	return func(key string) string {
		return vars[key]
	}
}

// Regression test: ConfigFilePath's default used to be computed
// independently of WorkingDir, so setting WORKING_DIR alone left
// config.json (and nothing else) still pointing at the real home
// directory. Db/Media/Thumbnails must track WorkingDir too.
func TestResolveDefaultsDerivesPathsFromWorkingDir(t *testing.T) {
	env := fakeEnv(map[string]string{"WORKING_DIR": "/srv/cutepi"})
	c := resolveDefaults(env, "/home/pi")

	if c.WorkingDir != "/srv/cutepi" {
		t.Fatalf("WorkingDir = %q, want /srv/cutepi", c.WorkingDir)
	}
	if c.ConfigFilePath != filepath.Join("/srv/cutepi", "config", "config.json") {
		t.Errorf("ConfigFilePath = %q, want it derived from WorkingDir", c.ConfigFilePath)
	}
	if c.Db.Location != filepath.Join("/srv/cutepi", "config", "ctp.db") {
		t.Errorf("Db.Location = %q, want it derived from WorkingDir", c.Db.Location)
	}
	if c.Media.Location != filepath.Join("/srv/cutepi", "media") {
		t.Errorf("Media.Location = %q, want it derived from WorkingDir", c.Media.Location)
	}
	if c.Thumbnails.Location != filepath.Join("/srv/cutepi", "thumbnails") {
		t.Errorf("Thumbnails.Location = %q, want it derived from WorkingDir", c.Thumbnails.Location)
	}
}

func TestResolveDefaultsWithNoEnvUsesHomeDir(t *testing.T) {
	c := resolveDefaults(fakeEnv(nil), "/home/pi")

	if c.WorkingDir != filepath.Join("/home/pi", "CTP") {
		t.Errorf("WorkingDir = %q, want under home dir", c.WorkingDir)
	}
	if c.Port != defaultPort {
		t.Errorf("Port = %d, want default %d", c.Port, defaultPort)
	}
	if c.PollInterval != defaultPollInterval {
		t.Errorf("PollInterval = %d, want default %d", c.PollInterval, defaultPollInterval)
	}
}

func TestResolveDefaultsExplicitOverridesWinOverWorkingDir(t *testing.T) {
	env := fakeEnv(map[string]string{
		"WORKING_DIR": "/srv/cutepi",
		"CONFIG_PATH": "/etc/cutepi/config.json",
		"DB_PATH":     "/data/ctp.db",
		"PORT":        "8080",
	})
	c := resolveDefaults(env, "/home/pi")

	if c.ConfigFilePath != "/etc/cutepi/config.json" {
		t.Errorf("CONFIG_PATH override ignored: got %q", c.ConfigFilePath)
	}
	if c.Db.Location != "/data/ctp.db" {
		t.Errorf("DB_PATH override ignored: got %q", c.Db.Location)
	}
	if c.Port != 8080 {
		t.Errorf("PORT override ignored: got %d", c.Port)
	}
	// Media/Thumbnails weren't overridden, so they should still follow
	// WorkingDir.
	if c.Media.Location != filepath.Join("/srv/cutepi", "media") {
		t.Errorf("Media.Location = %q, want it derived from WorkingDir", c.Media.Location)
	}
}

func TestResolveDefaultsExpandsTildeInOverrides(t *testing.T) {
	env := fakeEnv(map[string]string{"MEDIA_DIR": "~/my-media"})
	c := resolveDefaults(env, "/home/pi")

	if c.Media.Location != filepath.Join("/home/pi", "my-media") {
		t.Errorf("Media.Location = %q, want ~ expanded against home dir", c.Media.Location)
	}
}

func TestLoadConfigCreatesFileOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { useTestDir(testDir) })
	useTestDir(dir)

	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no config file before LoadConfig, stat err = %v", err)
	}

	LoadConfig()

	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("expected LoadConfig to create the config file on first run: %v", err)
	}
}

func TestLoadConfigClampsPollIntervalBelowMinimum(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { useTestDir(testDir) })
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"poll_interval_ms": 1}`), 0o644); err != nil {
		t.Fatalf("writing test config file: %v", err)
	}
	useTestDir(dir)

	LoadConfig()

	if PollInterval() < minPollInterval {
		t.Fatalf("PollInterval() = %d, want clamped to >= %d", PollInterval(), minPollInterval)
	}
}

// TestSaveConfigAtomic is the runnable check for the temp+rename save: the
// config file must remain valid JSON after a save (never truncated - the
// old os.Create truncate-then-encode lost the whole file on a crash or
// power-cut mid-write, and LoadConfig then silently reverted to defaults,
// dropping the operator's port/auth settings), and no .tmp sidecar is left
// behind.
func TestSaveConfigAtomic(t *testing.T) {
	dir := t.TempDir()
	SetConfigFilePath(filepath.Join(dir, "config.json"))
	SetDirsForTesting(dir)
	SetPort(5050)
	SetAuthPassword("s3cret")

	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("config file missing after save: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if parsed["auth_password"] != "s3cret" || parsed["port"].(float64) != 5050 {
		t.Fatalf("saved config lost settings: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json") + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp sidecar left behind: %v", err)
	}
}
