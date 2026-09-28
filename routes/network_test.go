package routes

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Hostname validation rejects everything hostnamectl would. Case is
// normalized, not rejected (hostnames are case-insensitive).
func TestSetInstanceNameValidation(t *testing.T) {
	for _, bad := range []string{"", "has space", "-lead", "trail-", "under_score", "dot.name", strings.Repeat("a", 64)} {
		if err := SetInstanceName(bad); err == nil {
			t.Errorf("SetInstanceName(%q) = nil, want error", bad)
		}
	}
}

// The rename runs hostnamectl + avahi refresh via PATH, so a stub proves
// the wiring without renaming the test machine.
func TestSetInstanceNameApplies(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	stub := "#!/bin/sh\necho \"$0 $@\" >> " + log + "\n"
	for _, name := range []string{"hostnamectl", "avahi-set-host-name"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := SetInstanceName("stage-pi-2"); err != nil {
		t.Fatalf("SetInstanceName: %v", err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("stubs never ran: %v", err)
	}
	calls := string(raw)
	if !strings.Contains(calls, "hostnamectl set-hostname stage-pi-2") {
		t.Errorf("hostnamectl not called to rename: %s", calls)
	}
	if !strings.Contains(calls, "avahi-set-host-name stage-pi-2") {
		t.Errorf("avahi refresh not called: %s", calls)
	}
}

// The endpoint surfaces validation failures instead of renaming.
func TestHostnameEndpointRejects(t *testing.T) {
	r := setupTestServer(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/setting/hostname", strings.NewReader("hostname=NOPE_bad"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST hostname=NOPE_bad = %d, want 400", w.Code)
	}
}
