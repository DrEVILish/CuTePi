package main

import (
	_ "embed"
	"strings"
)

// The app version, from VERSION at the repo root (semantic versioning; the
// release commit is tagged v<version>). Built into the binary.
//
//go:embed VERSION
var versionFile string

var version = strings.TrimSpace(versionFile)
