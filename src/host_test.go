package main

import "github.com/branchkit/plugin-sdk-go"

// newTestHost is a host on a detached plugin: no platform behind it, every
// platform call fails at once, mirrors never fetch. Tests exercise the
// plugin's own logic; a test that needs a platform answer installs a seam.
func newTestHost() *Host {
	h := newHost(branchkit.NewDetachedPlugin())
	// What the platform would answer on a machine with a tray: continuous
	// listening is allowed, and profile writes land.
	h.statusIndicatorAvailable = func() (bool, error) { return true, nil }
	h.persistAppProfiles = func(map[string]string) error { return nil }
	// Mode writes land, as they would against a platform. The detached plugin
	// would fail them and schedule a retry in every test that changes mode.
	h.writeMode = func(string) error { return nil }
	return h
}
