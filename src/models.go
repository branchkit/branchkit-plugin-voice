package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/branchkit/plugin-sdk-go"
)

// This plugin's own model namespace, read directly.
//
// Voice ships two engines, so "which of my models are installed?" is a question
// it has to answer constantly — for the picker, for the download buttons, for
// the setup prompt. It used to answer it by asking the platform: the platform
// scanned the directory, published the result as `models_on_disk` on the
// `_platform.model_info` collection, and the Swift shell held the selected-model
// state in between. All of that existed because this process could not open its
// own model directory. It can now (`branchkit.ModelsDir()`, read-only), so the
// question is a `ReadDir`.

// The two models a fresh install needs, named once. Both are declared in this
// plugin's `provides.models`, so `modelRef` turns either into the ref the CLI
// provisions from.
const (
	defaultDictationModel = "openai_whisper-large-v3-v20240930"
	defaultCommandModel   = "sherpa-offline-nemo"
)

// commandModel is one model the command stage can load, as Get started
// offers it. The manifest does not say which stage consumes an artifact,
// so the command models are listed here by name.
type commandModel struct {
	Name    string // the artifact name, also the stage's --model
	Label   string // what Get started calls it
	Note    string // why someone would pick it
	Title   string // the model itself, as the Models tab names it
	License string // its license, shown beside it
	Source  string // where the license and attribution are
}

// commandModels are the command models voice offers, the default first. Both
// are NVIDIA NeMo CTC checkpoints (CC BY 4.0) in BranchKit's conversion. The
// default scored best overall on synthetic and real-voice clips (2026-10-20);
// the small one is the lighter option.
var commandModels = []commandModel{
	{
		Name: defaultCommandModel, Label: "Standard", Note: "most accurate",
		Title: "NVIDIA NeMo FastConformer-CTC large", License: "CC BY 4.0",
		Source: "https://huggingface.co/branchkit/nemo-ctc-en-fastconformer-large-onnx",
	},
	{
		Name: "sherpa-nemo-small", Label: "Light", Note: "smaller and faster",
		Title: "NVIDIA NeMo Conformer-CTC small", License: "CC BY 4.0",
		Source: "https://huggingface.co/branchkit/nemo-ctc-en-conformer-small-onnx",
	},
}

// isCommandModel reports whether name is one of the command models.
func isCommandModel(name string) bool {
	for _, m := range commandModels {
		if m.Name == name {
			return true
		}
	}
	return false
}

// modelOnDisk reports whether this plugin's model namespace holds name. A
// stat, not installedModels: it runs on every command hold.
func modelOnDisk(name string) bool {
	root := branchkit.ModelsDir()
	if root == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(root, name))
	return err == nil && info.IsDir()
}

// commandModelOnDisk is the command model the stage can load now: the chosen
// one once it is downloaded, until then the first command model that is (the
// default first). Choosing a model that is still downloading must not leave
// the stage with a --model that is not there: commands would stop working
// until the download ends. With none on disk it is the chosen one, which Get
// started offers to download.
func commandModelOnDisk(chosen string, onDisk func(string) bool) string {
	if onDisk(chosen) {
		return chosen
	}
	for _, m := range commandModels {
		if onDisk(m.Name) {
			return m.Name
		}
	}
	return chosen
}

// resolveCommandModel is the model the command stage actually loads: the
// configured one when it is a command model, else the default. A setting
// holding anything else (a typo, a model since withdrawn) must not reach the
// stage as a --model nothing provides.
func resolveCommandModel(configured string) string {
	if isCommandModel(configured) {
		return configured
	}
	return defaultCommandModel
}

// InstalledModel is one model present in this plugin's namespace.
type InstalledModel struct {
	Name string
	Size string
}

// installedModels lists what is on disk under this plugin's model root, by
// name. An unreadable or absent root is an empty list, not an error: "no models
// installed yet" is the first-run state and reads the same way.
func installedModels() []InstalledModel {
	root := branchkit.ModelsDir()
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []InstalledModel
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		out = append(out, InstalledModel{
			Name: e.Name(),
			Size: humanDirSize(filepath.Join(root, e.Name())),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// installedModelSet is installedModels keyed by name, for the "is this one
// downloaded, and how big is it really" lookup the picker does per row.
func installedModelSet() map[string]string {
	set := make(map[string]string)
	for _, m := range installedModels() {
		set[m.Name] = m.Size
	}
	return set
}

// modelRef is the platform-wide name for one of this plugin's declared models:
// `<plugin id>/<model>`. That is what `model.download` and `model.delete` take,
// and it is derived from the plugin's own id rather than written down, because
// the vendor-named prefix this replaced (`whisperkit/…`) silently stopped
// resolving when models moved into per-plugin namespaces and every download
// button in this tab failed with "Unknown model".
func (h *Host) modelRef(name string) string {
	return h.plugin.ID() + "/" + name
}

// humanDirSize renders a directory's recursive byte total the way the picker
// shows it.
func humanDirSize(path string) string {
	var total int64
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	const gb = 1 << 30
	const mb = 1 << 20
	if total >= gb {
		return fmt.Sprintf("%.1f GB", float64(total)/float64(gb))
	}
	return fmt.Sprintf("%d MB", total/mb)
}
