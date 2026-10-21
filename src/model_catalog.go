package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The dictation-model catalog comes from THIS plugin's manifest, filtered
// to the running OS — not from a list written into Go. The catalog used to
// be three WhisperKit names hardcoded here, duplicating `provides.models`
// and showing a Linux or Windows user three models its engine (whisper.cpp)
// could not load. The manifest is the one place a model is declared; the
// CLI provisions from it and the sandbox grants from it, so the tab reads
// from it too (2026-09-18).

// declaredModel is the subset of a `provides.models` entry the tab shows.
type declaredModel struct {
	Name        string
	Description string
	SizeBytes   int64
	// Platform is the manifest's constraint: nil (every OS), or the names.
	Platform []string
	// Sources are the repositories its files are downloaded from
	// ("huggingface.co/<org>/<repo>"), one per distinct source, in part
	// order: a WhisperKit model's Core ML weights and OpenAI's tokenizer
	// files come from two.
	Sources []string
}

// License is every distinct license of m's sources, in order, joined with
// " + "; "" when a source's license is not known (shown as none, not
// guessed).
func (m declaredModel) License() string {
	var out []string
	seen := map[string]bool{}
	for _, s := range m.Sources {
		l := sourceLicenses[s]
		if l == "" {
			return ""
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return strings.Join(out, " + ")
}

// currentPlatformName is the manifest spelling of runtime.GOOS.
// partSource is the slice of an artifact part that says where it comes from:
// a Hugging Face repo (hf_folder, hf_files) or a URL (http_file,
// http_archive).
type partSource struct {
	Repo string `json:"repo"`
	URL  string `json:"url"`
}

// sourcesOf is the distinct repositories parts download from, in order: a
// Hugging Face repo as "huggingface.co/<org>/<repo>", any other URL as its
// host and path. A part naming neither (a plugin_file) adds none.
func sourcesOf(parts []partSource) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range parts {
		s := sourceOf(p)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sourceOf(p partSource) string {
	if p.Repo != "" {
		return "huggingface.co/" + p.Repo
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Host == "" {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Host == "huggingface.co" && len(segs) >= 2 {
		return "huggingface.co/" + segs[0] + "/" + segs[1]
	}
	return u.Host + u.Path
}

// sourceLicenses are the licenses of the repositories voice downloads models
// from, read from each repository's model card (2026-10-20). A source not
// listed shows no license rather than a guessed one.
var sourceLicenses = map[string]string{
	"huggingface.co/branchkit/nemo-ctc-en-fastconformer-large-onnx": "CC BY 4.0",
	"huggingface.co/branchkit/nemo-ctc-en-conformer-small-onnx":     "CC BY 4.0",
	"huggingface.co/argmaxinc/whisperkit-coreml":                    "MIT",
	"huggingface.co/ggerganov/whisper.cpp":                          "MIT",
	"huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF":                "Apache-2.0",
	"huggingface.co/Qwen/Qwen3-4B-GGUF":                             "Apache-2.0",
	"huggingface.co/openai/whisper-large-v3":                        "Apache-2.0",
	"huggingface.co/openai/whisper-small.en":                        "Apache-2.0",
	"huggingface.co/openai/whisper-base.en":                         "Apache-2.0",
	// Silero VAD, which every command model's directory carries.
	"github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/silero_vad.onnx": "MIT",
}

func currentPlatformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}

func (m declaredModel) forThisOS() bool {
	if len(m.Platform) == 0 {
		return true
	}
	cur := currentPlatformName()
	for _, p := range m.Platform {
		if p == cur {
			return true
		}
	}
	return false
}

// declaredModels reads `provides.models` from the plugin's own manifest, in
// MANIFEST ORDER — the first entry for an OS is that OS's default, so order
// is meaning and a Go map would lose it. An unreadable manifest is an empty
// catalog, which the tab renders as "nothing to download here".
func declaredModels() []declaredModel {
	dir := os.Getenv("BRANCHKIT_PLUGIN_DIR")
	if dir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil
	}
	var top struct {
		Provides struct {
			Artifacts json.RawMessage `json:"artifacts"`
			Models    json.RawMessage `json:"models"` // deprecated alias, one release
		} `json:"provides"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	models := top.Provides.Artifacts
	if len(models) == 0 {
		models = top.Provides.Models // deprecated alias
	}
	if len(models) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(models))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var out []declaredModel
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		name, _ := keyTok.(string)
		var entry struct {
			Description string          `json:"description"`
			SizeBytes   int64           `json:"size_bytes"`
			Platform    json.RawMessage `json:"platform"`
			Parts       []partSource    `json:"parts"`
		}
		if err := dec.Decode(&entry); err != nil {
			break
		}
		m := declaredModel{Name: name, Description: entry.Description, SizeBytes: entry.SizeBytes, Sources: sourcesOf(entry.Parts)}
		if len(entry.Platform) > 0 {
			var one string
			if json.Unmarshal(entry.Platform, &one) == nil {
				m.Platform = []string{one}
			} else {
				_ = json.Unmarshal(entry.Platform, &m.Platform)
			}
		}
		out = append(out, m)
	}
	return out
}

// catalogForThisOS is the declared models this OS can load, in manifest
// order — every role, so the command model is in here too.
func catalogForThisOS() []declaredModel {
	var out []declaredModel
	for _, m := range declaredModels() {
		if m.forThisOS() {
			out = append(out, m)
		}
	}
	return out
}

// dictationCatalog is the dictation engine's share of catalogForThisOS: the
// command models are left out by name. The manifest says which OS a model is
// for but not which stage consumes it, and the command model is declared
// for every OS, so it is the FIRST entry the platform filter lets through
// on Linux and Windows. Before this exclusion that made it the dictation
// default there: the whisper stage was spawned with `--model
// sherpa-offline-nemo` and reported "no whisper model found" while the
// declared ggml model sat provisioned one directory over (2026-09-18, Linux
// VM). Declaring the consuming role in the manifest — so no plugin filters
// by name — is still to do.
func dictationCatalog() []declaredModel {
	var out []declaredModel
	for _, m := range catalogForThisOS() {
		// The command, refine and style models are artifacts of other
		// stages; the manifest does not record which stage loads which.
		if isCommandModel(m.Name) || m.Name == refineModel || m.Name == styleModel {
			continue
		}
		out = append(out, m)
	}
	return out
}

// resolveDictationModel is the model the dictation pipeline actually loads:
// the configured one when this OS's engine can run it, else this OS's
// default — the first model declared for it. The configured name comes from
// a setting whose declared default is the macOS model; on Linux and Windows
// that name means nothing to whisper.cpp, and passing it through was how
// dictation there named a model no grant reached.
func resolveDictationModel(configured string) string {
	catalog := dictationCatalog()
	for _, m := range catalog {
		if m.Name == configured {
			return configured
		}
	}
	if len(catalog) > 0 {
		return catalog[0].Name
	}
	return configured
}

// humanBytes renders a declared size the way the picker shows it.
func humanBytes(n int64) string {
	const gb = 1 << 30
	const mb = 1 << 20
	if n >= gb {
		return fmt.Sprintf("%.2f GB", float64(n)/float64(gb))
	}
	return fmt.Sprintf("%d MB", n/mb)
}
