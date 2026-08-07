package main

import "github.com/danielbecerra/corral/internal/version"

// versionString is a thin wrapper so main.go doesn't need to import
// internal/version directly at multiple call sites and tests can stub it if
// ever needed.
func versionString() string {
	return version.String()
}
