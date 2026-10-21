package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeManifest(t *testing.T, models string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"),
		[]byte(`{"id":"voice","provides":{"models":`+models+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRANCHKIT_PLUGIN_DIR", dir)
}

func TestCatalogKeepsManifestOrderAndFiltersByPlatform(t *testing.T) {
	writeManifest(t, `{
		"mac-large": {"size_bytes": 3, "platform": "macos"},
		"cpp-base":  {"size_bytes": 1, "platform": ["linux","windows"]},
		"cpp-small": {"size_bytes": 2, "platform": ["linux","windows"]},
		"anywhere":  {"size_bytes": 4}
	}`)
	all := declaredModels()
	if len(all) != 4 || all[0].Name != "mac-large" || all[3].Name != "anywhere" {
		t.Fatalf("manifest order lost: %+v", all)
	}
	got := catalogForThisOS()
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	want := map[string][]string{
		"darwin":  {"mac-large", "anywhere"},
		"linux":   {"cpp-base", "cpp-small", "anywhere"},
		"windows": {"cpp-base", "cpp-small", "anywhere"},
	}[runtime.GOOS]
	if len(names) != len(want) {
		t.Fatalf("catalog for %s = %v, want %v", runtime.GOOS, names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("catalog for %s = %v, want %v", runtime.GOOS, names, want)
		}
	}
}

func TestResolveDictationModelFallsBackToThisOSDefault(t *testing.T) {
	writeManifest(t, `{
		"mac-large": {"size_bytes": 3, "platform": "macos"},
		"cpp-base":  {"size_bytes": 1, "platform": ["linux","windows"]},
		"cpp-small": {"size_bytes": 2, "platform": ["linux","windows"]}
	}`)
	def := map[string]string{"darwin": "mac-large", "linux": "cpp-base", "windows": "cpp-base"}[runtime.GOOS]
	// A model this OS's engine cannot load resolves to the OS default…
	other := map[string]string{"darwin": "cpp-small", "linux": "mac-large", "windows": "mac-large"}[runtime.GOOS]
	if got := resolveDictationModel(other); got != def {
		t.Fatalf("resolve(%q) = %q, want the OS default %q", other, got, def)
	}
	// …a declared one for this OS passes through…
	if got := resolveDictationModel(def); got != def {
		t.Fatalf("resolve(%q) = %q", def, got)
	}
	// …and with no manifest at all the configured name is left alone.
	t.Setenv("BRANCHKIT_PLUGIN_DIR", "")
	if got := resolveDictationModel("whatever"); got != "whatever" {
		t.Fatalf("resolve without a manifest = %q", got)
	}
}

// The command models are declared for every OS, so the platform filter alone
// puts them FIRST in the Linux/Windows catalog — and "first" is what the
// dictation fallback picks. This is the regression: the whisper stage got
// `--model sherpa-offline-nemo`.
func TestDictationCatalogLeavesOutTheCommandModels(t *testing.T) {
	writeManifest(t, `{
		"`+defaultCommandModel+`": {"size_bytes": 9},
		"sherpa-nemo-small": {"size_bytes": 8},
		"mac-large": {"size_bytes": 3, "platform": "macos"},
		"cpp-base":  {"size_bytes": 1, "platform": ["linux","windows"]}
	}`)
	for _, m := range dictationCatalog() {
		if isCommandModel(m.Name) {
			t.Fatalf("dictation catalog lists a command model: %+v", dictationCatalog())
		}
	}
	// catalogForThisOS still has it: the Models tab sizes the recommended
	// download from the full list.
	found := false
	for _, m := range catalogForThisOS() {
		found = found || m.Name == defaultCommandModel
	}
	if !found {
		t.Fatalf("full catalog lost the command model: %+v", catalogForThisOS())
	}
	def := map[string]string{"darwin": "mac-large", "linux": "cpp-base", "windows": "cpp-base"}[runtime.GOOS]
	if got := resolveDictationModel("something-this-os-cannot-load"); got != def {
		t.Fatalf("dictation fallback = %q, want %q", got, def)
	}
	if got := resolveDictationModel(defaultCommandModel); got != def {
		t.Fatalf("configuring the command model for dictation resolved to %q, want %q", got, def)
	}
}
