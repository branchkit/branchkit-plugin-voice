package main

import "testing"

func TestCurrentAppScope(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"app.com.google.Chrome":         "app.com.google.Chrome",
		"plugin.voice.mode, app.com.x":  "app.com.x",
		"plugin.browser.hints":          "",
		" app.com.y , plugin.voice.foo": "app.com.y",
	}
	for in, want := range cases {
		if got := currentAppScope(in); got != want {
			t.Errorf("currentAppScope(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetAppScope(t *testing.T) {
	// Add scope to a bare requires string.
	if got := setAppScope("", "app.com.google.Chrome"); got != "app.com.google.Chrome" {
		t.Errorf("add to empty: got %q", got)
	}
	// Replace an existing app scope, preserving non-app tags (order: non-app then app).
	got := setAppScope("plugin.voice.mode, app.com.old", "app.com.new")
	if got != "plugin.voice.mode,app.com.new" {
		t.Errorf("replace: got %q, want %q", got, "plugin.voice.mode,app.com.new")
	}
	// Clearing scope ("Any app") drops the app tag but keeps other conditions.
	if got := setAppScope("plugin.voice.mode, app.com.x", ""); got != "plugin.voice.mode" {
		t.Errorf("clear: got %q, want %q", got, "plugin.voice.mode")
	}
	// Clearing with no other tags yields empty.
	if got := setAppScope("app.com.x", ""); got != "" {
		t.Errorf("clear only: got %q, want \"\"", got)
	}
}

func TestBuildAppOptions(t *testing.T) {
	apps := []AppEntry{
		{BundleID: "com.google.Chrome", Name: "chrome"},
		{BundleID: "com.apple.Safari", Name: "safari"},
	}
	// Any-app default selected when no scope.
	opts := buildAppOptions(apps, "")
	if opts[0].Value != "" || !opts[0].Selected {
		t.Errorf("first option should be selected Any app, got %+v", opts[0])
	}
	if len(opts) != 3 {
		t.Errorf("want Any app + 2 apps = 3, got %d", len(opts))
	}
	// A known scope is selected.
	opts = buildAppOptions(apps, "app.com.apple.Safari")
	var sel string
	for _, o := range opts {
		if o.Selected {
			sel = o.Value
		}
	}
	if sel != "app.com.apple.Safari" {
		t.Errorf("selected = %q, want safari tag", sel)
	}
	// An uninstalled scope is kept selectable so it round-trips.
	opts = buildAppOptions(apps, "app.com.notinstalled")
	last := opts[len(opts)-1]
	if last.Value != "app.com.notinstalled" || !last.Selected {
		t.Errorf("uninstalled scope should be kept selected, got %+v", last)
	}
}
