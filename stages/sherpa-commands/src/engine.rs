// Thin sherpa-onnx-sys wrapper for the offline command stage.
//
// Keeps the raw offline recognizer pointer reachable so we can call the patched
// `SherpaOnnxCreateOfflineRecognizerWithFstGraph` /
// `SherpaOnnxOfflineRecognizerBuildGraphFromWords` + `...InstallGraph` (the
// in-process HL grammar build, then swap), which the upstream safe crate does
// not expose. The patched symbols come from the grammar-provider overlay libs linked via
// SHERPA_ONNX_LIB_DIR (see stages/sherpa-commands/patches/).

use std::ffi::{CStr, CString};
use std::os::raw::c_char;

use serde_json::Value;
// ---------------------------------------------------------------------------
// Offline (encode-once) recognizer + silero VAD — the non-streaming command path.
//
// The offline CTC+HLG decoder reads the whole utterance with full right-context
// (no chunk-16 settle floor) and decodes once per VAD-bounded segment. The
// closed grammar is the same HL.fst family as the online path. The C-API does
// NOT expose the offline FST graph field, so the recognizer is built via the
// patched `SherpaOnnxCreateOfflineRecognizerWithFstGraph` (grammar-provider
// overlay); everything else uses the stock -sys offline + VAD bindings.
// ---------------------------------------------------------------------------

use sherpa_onnx_sys::{
    OfflineModelConfig, OfflineRecognizer as SysOfflineRecognizer, OfflineRecognizerConfig,
    SileroVadModelConfig, SpeechSegment, VadModelConfig,
    VoiceActivityDetector as SysVad, SherpaOnnxAcceptWaveformOffline,
    SherpaOnnxCreateOfflineStream, SherpaOnnxCreateVoiceActivityDetector,
    SherpaOnnxDecodeOfflineStream, SherpaOnnxDestroyOfflineRecognizer,
    SherpaOnnxDestroyOfflineStream, SherpaOnnxDestroyOfflineStreamResultJson,
    SherpaOnnxDestroySpeechSegment, SherpaOnnxDestroyVoiceActivityDetector,
    SherpaOnnxGetOfflineStreamResultAsJson, SherpaOnnxVoiceActivityDetectorAcceptWaveform,
    SherpaOnnxVoiceActivityDetectorDetected, SherpaOnnxVoiceActivityDetectorEmpty,
    SherpaOnnxVoiceActivityDetectorFlush,
    SherpaOnnxVoiceActivityDetectorFront, SherpaOnnxVoiceActivityDetectorPop,
    SherpaOnnxVoiceActivityDetectorReset,
};

// Patched C-API symbol (not in stock -sys): create an offline recognizer pinned
// to a closed HL/HLG decoding graph (selects OfflineCtcFstDecoder). Mirrors the
// online SetGraphFromWords function-not-struct-field approach so the stock -sys
// OfflineRecognizerConfig layout is untouched.
unsafe extern "C" {
    fn SherpaOnnxCreateOfflineRecognizerWithFstGraph(
        config: *const OfflineRecognizerConfig,
        graph: *const c_char,
        max_active: i32,
    ) -> *const SysOfflineRecognizer;

    // [Async grammar swap] SetGraphFromWords / SetGraphFromDag split in two:
    // a Build* composes the graph and returns it without
    // touching decoding state, so it runs on a worker thread while this
    // recognizer decodes; InstallGraph swaps it onto the decoder (a pointer
    // swap) and consumes the handle.
    //
    // BuildGraphFromWords: an HL grammar from a word list (Direction 2 on
    // offline). `weights` is null (unweighted) or a parallel array of
    // `num_words` per-word decoding biases (weighted-WFST shallow
    // fusion): 0 neutral, positive penalizes, negative boosts.
    fn SherpaOnnxOfflineRecognizerBuildGraphFromWords(
        recognizer: *const SysOfflineRecognizer,
        words: *const *const c_char,
        num_words: i32,
        weights: *const f32,
    ) -> *mut SherpaOnnxOfflineGraph;

    // [Command-grammar compiler — D2] BuildGraphFromDag: a STRUCTURED grammar
    // (H o L o G) from a word-level pattern-DAG. The DAG (the platform's D1
    // compile of every cmd.pattern) constrains word *sequences*, so a word is
    // reachable only where a command places it — "scroll eight" becomes an
    // unwalkable path. Carried as parallel arc arrays; see the C-API header.
    // `arc_weight` null = unweighted; `open_states` flags unconstrained tails;
    // `open_alphabet_counts` is parallel to `open_states` with the words
    // flattened in that order (a count of 0, or null arrays, = loop over the
    // full union).
    fn SherpaOnnxOfflineRecognizerBuildGraphFromDag(
        recognizer: *const SysOfflineRecognizer,
        num_states: i32,
        start: i32,
        arc_from: *const i32,
        arc_word: *const *const c_char,
        arc_to: *const i32,
        arc_weight: *const f32,
        num_arcs: i32,
        open_states: *const i32,
        num_open: i32,
        open_alphabet_words: *const *const c_char,
        open_alphabet_counts: *const i32,
        union_words: *const *const c_char,
        num_union: i32,
    ) -> *mut SherpaOnnxOfflineGraph;

    fn SherpaOnnxOfflineRecognizerInstallGraph(
        recognizer: *const SysOfflineRecognizer,
        graph: *mut SherpaOnnxOfflineGraph,
    );

    fn SherpaOnnxDestroyOfflineGraph(graph: *mut SherpaOnnxOfflineGraph);

    // [Interactive inference threads] Every onnxruntime worker thread of a
    // session created after the call starts at the OS's interactive priority
    // (macOS QOS_CLASS_USER_INTERACTIVE, Windows ABOVE_NORMAL); 0 = this OS
    // has no supported raise (Linux). The raise lives in onnxruntime's
    // thread-creation hook because a new thread does not inherit its
    // creator's QoS on macOS (probed 2026-10-08).
    fn SherpaOnnxSetInteractiveOrtThreads(enabled: i32) -> i32;
    fn SherpaOnnxRaiseCurrentThreadToInteractive() -> i32;
    fn SherpaOnnxInteractiveOrtThreadStats(created: *mut i32, raised: *mut i32);
}

/// Opaque C graph handle (`SherpaOnnxOfflineGraph`).
#[repr(C)]
struct SherpaOnnxOfflineGraph {
    _private: [u8; 0],
}

/// Ask onnxruntime to create its worker threads at interactive priority for
/// every session created from now on. Call before [`OfflineRecognizer::create`].
/// False where the OS has no supported raise.
pub fn enable_interactive_inference_threads() -> bool {
    unsafe { SherpaOnnxSetInteractiveOrtThreads(1) != 0 }
}

/// Raise the calling thread to interactive priority. The thread that decodes
/// runs part of every encode (onnxruntime's caller joins its intra-op pool),
/// so it needs the raise as much as the workers. True when it reads back.
pub fn raise_current_thread_to_interactive() -> bool {
    unsafe { SherpaOnnxRaiseCurrentThreadToInteractive() != 0 }
}

/// `(created, raised)`: onnxruntime worker threads created through the
/// interactive hook, and how many of them read back as raised from inside
/// the thread (macOS: `pthread_get_qos_class_np`).
pub fn interactive_inference_thread_stats() -> (i32, i32) {
    let (mut created, mut raised) = (0i32, 0i32);
    unsafe { SherpaOnnxInteractiveOrtThreadStats(&mut created, &mut raised) };
    (created, raised)
}

/// A decoding graph built off the decode thread, not yet installed. Dropping
/// it frees the graph (a superseded build).
pub struct BuiltGraph(*mut SherpaOnnxOfflineGraph);

// SAFETY: the handle owns a self-contained graph that no other object
// references until it is installed; moving it between threads is moving
// ownership of plain heap data.
unsafe impl Send for BuiltGraph {}

impl Drop for BuiltGraph {
    fn drop(&mut self) {
        unsafe { SherpaOnnxDestroyOfflineGraph(self.0) }
    }
}

/// A decode's full readout: the cleaned 1-best `text`, plus the per-token BPE
/// pieces (`tokens`), their onset times (`timestamps`), and — via the engine
/// posteriors patch on the overlay — the per-token argmax `margins[i]`: the
/// token's peak-frame acoustic log-prob minus the best competing symbol on that
/// frame (positive = the token was the frame argmax, negative = the closed
/// grammar coerced the path against the acoustic evidence). Parallel to
/// `tokens` (including `<blk>` entries on the NeMo path — filter alongside the
/// blank skip when grouping to words). The margin is the statistic that
/// separates genuine from coerced (spike 2026-08-08); raw peak log-probs proved
/// peaky-compressed and are no longer surfaced. Used by the `probe` path for
/// calibration diagnostics; the live path uses the cheaper `decode` (text only).
#[derive(Default)]
pub struct DecodeDetail {
    pub text: String,
    pub tokens: Vec<String>,
    pub timestamps: Vec<f32>,
    pub margins: Vec<f32>,
}

/// Offline CTC+HLG recognizer (raw sherpa-onnx handle). Decodes one buffered
/// utterance per call with full bidirectional context.
pub struct OfflineRecognizer {
    ptr: *const SysOfflineRecognizer,
    // NeMo's tokenizer case-folds (lowercase-trained), but the runtime in-process
    // grammar builder uses ssentencepiece over bpe.vocab, which does NOT case-fold
    // and throws on chars absent from its lowercase vocab. So uppercase command
    // words must be lowercased before a runtime swap. The static build_hlg.py path
    // is unaffected (it uses the real SentencePiece, which case-folds). zipformer's
    // BPE is uppercase, so this stays off there. clean_offline_text re-uppercases.
    lowercase_words: bool,
}

// SAFETY: the recognizer is shared with exactly one grammar-build worker
// (offline.rs). That worker only calls the Build* entry points, which touch
// the C++ grammar builder alone, under a mutex the C++ side holds for every
// build; decoding and InstallGraph stay on the stage's decode thread. The
// split was written for this, and no other method is called
// from a second thread.
unsafe impl Send for OfflineRecognizer {}
unsafe impl Sync for OfflineRecognizer {}

impl OfflineRecognizer {
    /// Build the offline recognizer over `graph` (the HL.fst path). The model is
    /// an offline zipformer-CTC ONNX; `max_active` bounds the FST search beam.
    pub fn create(onnx: &str, tokens: &str, graph: &str, max_active: i32) -> Option<Self> {
        let onnx_c = cstr(onnx);
        let tokens_c = cstr(tokens);
        let graph_c = cstr(graph);
        let provider_c = cstr("cpu");
        let method_c = cstr("greedy_search");
        let nemo_type_c = cstr("nemo_ctc");

        // The blank id selects the offline CTC architecture we ship: the zipformer-CTC
        // model is `<blk> 0`, NeMo Conformer-CTC is `<blk> 1024`. sherpa routes the
        // model through a different impl per config field, but both decode through the
        // same OfflineCtcFstDecoder (the closed HL graph), so only the model field +
        // model_type differ. The HL itself is built blank-id-aware (build_hlg.py).
        let blank_id = blank_id_from_tokens(tokens);

        // Zero-init the large offline model/recognizer config: every string field
        // is a `*const c_char`, and the C-API treats a null pointer as "" (the
        // SHERPA_ONNX_OR macro), so only the fields we set matter. feat_config
        // sample_rate/feature_dim of 0 default to 16000/80 the same way.
        let mut model_config: OfflineModelConfig = unsafe { std::mem::zeroed() };
        model_config.tokens = tokens_c.as_ptr();
        model_config.num_threads = 2;
        model_config.provider = provider_c.as_ptr();
        if blank_id == 0 {
            model_config.zipformer_ctc.model = onnx_c.as_ptr();
        } else {
            model_config.nemo_ctc.model = onnx_c.as_ptr();
            model_config.model_type = nemo_type_c.as_ptr();
        }

        let mut config: OfflineRecognizerConfig = unsafe { std::mem::zeroed() };
        config.feat_config.sample_rate = 16000;
        config.feat_config.feature_dim = 80;
        config.model_config = model_config;
        config.decoding_method = method_c.as_ptr();

        let ptr = unsafe {
            SherpaOnnxCreateOfflineRecognizerWithFstGraph(&config, graph_c.as_ptr(), max_active)
        };
        if ptr.is_null() {
            None
        } else {
            Some(OfflineRecognizer { ptr, lowercase_words: blank_id != 0 })
        }
    }

    /// Encode-once: decode a complete buffered segment into its recognized text.
    /// Text-only sugar over [`Self::decode_detail`] (the single FFI decode site,
    /// which the live path now calls directly for its margin readout); kept for
    /// the fixture tests below, which only assert on text.
    #[cfg(test)]
    pub fn decode(&self, samples: &[f32]) -> String {
        self.decode_detail(samples).text
    }

    /// Encode-once decode returning the full readout: cleaned text plus the
    /// per-token BPE pieces and onset timestamps the result JSON carries on the FST
    /// path. Creates a throwaway offline stream (the recognizer is reusable across
    /// segments; the stream is per-utterance). The single FFI site for an offline
    /// decode — `decode` delegates here for its text.
    pub fn decode_detail(&self, samples: &[f32]) -> DecodeDetail {
        unsafe {
            let stream = SherpaOnnxCreateOfflineStream(self.ptr);
            SherpaOnnxAcceptWaveformOffline(stream, 16000, samples.as_ptr(), samples.len() as i32);
            SherpaOnnxDecodeOfflineStream(self.ptr, stream);
            let json = SherpaOnnxGetOfflineStreamResultAsJson(stream);
            let detail = if json.is_null() {
                DecodeDetail::default()
            } else {
                CStr::from_ptr(json)
                    .to_str()
                    .ok()
                    .and_then(|s| serde_json::from_str::<Value>(s).ok())
                    .map(|v| DecodeDetail {
                        text: v
                            .get("text")
                            .and_then(|t| t.as_str())
                            .map(|s| clean_offline_text(s.to_string()))
                            .unwrap_or_default(),
                        tokens: v
                            .get("tokens")
                            .and_then(|t| t.as_array())
                            .map(|a| a.iter().filter_map(|x| x.as_str().map(String::from)).collect())
                            .unwrap_or_default(),
                        timestamps: v
                            .get("timestamps")
                            .and_then(|t| t.as_array())
                            .map(|a| a.iter().filter_map(|x| x.as_f64().map(|f| f as f32)).collect())
                            .unwrap_or_default(),
                        margins: v
                            .get("margins")
                            .and_then(|t| t.as_array())
                            .map(|a| a.iter().filter_map(|x| x.as_f64().map(|f| f as f32)).collect())
                            .unwrap_or_default(),
                    })
                    .unwrap_or_default()
            };
            SherpaOnnxDestroyOfflineStreamResultJson(json);
            SherpaOnnxDestroyOfflineStream(stream);
            detail
        }
    }

    /// Build an HL grammar from `words` in-process and swap it onto the
    /// recognizer's FST decoder: [`Self::build_graph_from_words`] then
    /// [`Self::install_graph`] on the calling thread. The next decode uses the
    /// new grammar; call it between segments.
    pub fn set_graph_from_words(&self, words: &[String], weights: &[f32]) {
        if let Some(g) = self.build_graph_from_words(words, weights) {
            self.install_graph(g);
        }
    }

    /// Build an HL grammar from `words` without installing it (the Direction
    /// 2 runtime grammar rebuild on the offline engine). Safe to call on a
    /// thread other than the one decoding. `None` when the recognizer has no
    /// CTC FST decoder or `words` is empty.
    ///
    /// `weights` carries the optional per-word decoding bias. Empty =
    /// unweighted; otherwise it must be parallel to `words` (0 neutral, positive
    /// penalizes, negative boosts). Lowercasing `words` here is positional, so the
    /// weights still line up after the fold; a length mismatch falls back to no
    /// bias rather than reading out of bounds on the C side.
    pub fn build_graph_from_words(&self, words: &[String], weights: &[f32]) -> Option<BuiltGraph> {
        let cstrings: Vec<CString> = words
            .iter()
            .map(|w| if self.lowercase_words { cstr(&w.to_lowercase()) } else { cstr(w) })
            .collect();
        let ptrs: Vec<*const c_char> = cstrings.iter().map(|c| c.as_ptr()).collect();
        let weights_ptr = if !weights.is_empty() && weights.len() == words.len() {
            weights.as_ptr()
        } else {
            std::ptr::null()
        };
        let g = unsafe {
            SherpaOnnxOfflineRecognizerBuildGraphFromWords(
                self.ptr,
                ptrs.as_ptr(),
                ptrs.len() as i32,
                weights_ptr,
            )
        };
        (!g.is_null()).then_some(BuiltGraph(g))
    }

    /// Swap a built graph onto the decoder — a pointer swap, so the next
    /// decode uses it. Call on the decode thread, between decodes.
    pub fn install_graph(&self, graph: BuiltGraph) {
        let g = std::mem::ManuallyDrop::new(graph);
        // Install consumes the handle; ManuallyDrop keeps Drop from freeing
        // it a second time.
        unsafe { SherpaOnnxOfflineRecognizerInstallGraph(self.ptr, g.0) }
    }

    /// Build a STRUCTURED grammar (`H ∘ L ∘ G`) from a word-level pattern-DAG
    /// in-process and swap it onto the recognizer (command-grammar compiler D2).
    /// Unlike [`set_graph_from_words`], the DAG constrains word *sequences*: a
    /// word is decodable only where a command places it, so a leading/contextual
    /// mishear ("scroll eight") is structurally unproducible. Words are
    /// lowercased positionally (per arc) to match the model's case-folded BPE,
    /// exactly as the word-list path does. Build then install, on the calling
    /// thread.
    pub fn set_graph_from_dag(
        &self,
        dag: &branchkit_stage_sdk::grammar_dag::GrammarDagWire,
        union_words: &[String],
    ) {
        if let Some(g) = self.build_graph_from_dag(dag, union_words) {
            self.install_graph(g);
        }
    }

    /// The build half of [`Self::set_graph_from_dag`]: safe off the decode
    /// thread, `None` when there is nothing to install.
    pub fn build_graph_from_dag(
        &self,
        dag: &branchkit_stage_sdk::grammar_dag::GrammarDagWire,
        union_words: &[String],
    ) -> Option<BuiltGraph> {
        let fold = |w: &str| {
            if self.lowercase_words {
                cstr(&w.to_lowercase())
            } else {
                cstr(w)
            }
        };
        let arc_words: Vec<CString> = dag.arcs.iter().map(|a| fold(&a.word)).collect();
        let arc_word_ptrs: Vec<*const c_char> = arc_words.iter().map(|c| c.as_ptr()).collect();
        let arc_from: Vec<i32> = dag.arcs.iter().map(|a| a.from as i32).collect();
        let arc_to: Vec<i32> = dag.arcs.iter().map(|a| a.to as i32).collect();
        let has_weights = dag.arcs.iter().any(|a| a.weight.is_some());
        let arc_weight: Vec<f32> = if has_weights {
            dag.arcs.iter().map(|a| a.weight.unwrap_or(0.0)).collect()
        } else {
            Vec::new()
        };
        let open_states: Vec<i32> = dag.open_states.iter().map(|o| o.state as i32).collect();
        // Per-open-state bounded alphabet (phase 3+): flatten each state's words
        // (folded to match word2id) in open_states order, with a parallel count
        // array. A None alphabet contributes count 0 → C++ loops the full union.
        let mut open_alpha_c: Vec<CString> = Vec::new();
        let open_alpha_counts: Vec<i32> = dag
            .open_states
            .iter()
            .map(|o| match &o.alphabet {
                Some(words) => {
                    for w in words {
                        open_alpha_c.push(fold(w));
                    }
                    words.len() as i32
                }
                None => 0,
            })
            .collect();
        let open_alpha_ptrs: Vec<*const c_char> = open_alpha_c.iter().map(|c| c.as_ptr()).collect();
        let union_c: Vec<CString> = union_words.iter().map(|w| fold(w)).collect();
        let union_ptrs: Vec<*const c_char> = union_c.iter().map(|c| c.as_ptr()).collect();
        let g = unsafe {
            SherpaOnnxOfflineRecognizerBuildGraphFromDag(
                self.ptr,
                dag.num_states as i32,
                dag.start as i32,
                arc_from.as_ptr(),
                arc_word_ptrs.as_ptr(),
                arc_to.as_ptr(),
                if arc_weight.is_empty() {
                    std::ptr::null()
                } else {
                    arc_weight.as_ptr()
                },
                arc_from.len() as i32,
                if open_states.is_empty() {
                    std::ptr::null()
                } else {
                    open_states.as_ptr()
                },
                open_states.len() as i32,
                if open_alpha_ptrs.is_empty() {
                    std::ptr::null()
                } else {
                    open_alpha_ptrs.as_ptr()
                },
                if open_alpha_counts.is_empty() {
                    std::ptr::null()
                } else {
                    open_alpha_counts.as_ptr()
                },
                if union_ptrs.is_empty() {
                    std::ptr::null()
                } else {
                    union_ptrs.as_ptr()
                },
                union_ptrs.len() as i32,
            )
        };
        (!g.is_null()).then_some(BuiltGraph(g))
    }
}

impl Drop for OfflineRecognizer {
    fn drop(&mut self) {
        unsafe { SherpaOnnxDestroyOfflineRecognizer(self.ptr) }
    }
}

/// silero VAD wrapper (neural speech-segment detector). The stage feeds audio in
/// window-sized chunks; the VAD emits a `SpeechSegment` (absolute start index +
/// samples) once a pause ≥ min_silence closes a segment. The stage re-expands
/// each segment with a pre-roll (silero trims fricative onsets) before encoding.
pub struct Vad {
    ptr: *const SysVad,
}

/// A detected speech segment: `start` is the absolute sample index into the audio
/// fed to the VAD since the last reset; `samples` are the in-segment PCM.
pub struct VadSegment {
    pub start: i64,
    pub samples: Vec<f32>,
}

impl Vad {
    pub fn create(
        model: &str,
        threshold: f32,
        min_silence: f32,
        min_speech: f32,
        window: i32,
        sample_rate: i32,
        max_speech: f32,
        buffer_seconds: f32,
    ) -> Option<Self> {
        let model_c = cstr(model);
        let provider_c = cstr("cpu");
        let silero = SileroVadModelConfig {
            model: model_c.as_ptr(),
            threshold,
            min_silence_duration: min_silence,
            min_speech_duration: min_speech,
            window_size: window,
            max_speech_duration: max_speech,
        };
        let mut config: VadModelConfig = unsafe { std::mem::zeroed() };
        config.silero_vad = silero;
        config.sample_rate = sample_rate;
        config.num_threads = 1;
        config.provider = provider_c.as_ptr();

        let ptr = unsafe { SherpaOnnxCreateVoiceActivityDetector(&config, buffer_seconds) };
        if ptr.is_null() {
            None
        } else {
            Some(Vad { ptr })
        }
    }

    pub fn accept(&self, samples: &[f32]) {
        unsafe {
            SherpaOnnxVoiceActivityDetectorAcceptWaveform(
                self.ptr,
                samples.as_ptr(),
                samples.len() as i32,
            );
        }
    }

    pub fn is_empty(&self) -> bool {
        unsafe { SherpaOnnxVoiceActivityDetectorEmpty(self.ptr) != 0 }
    }

    /// The front speech segment (copied out), or `None` if the queue is empty.
    /// Does NOT pop — call [`Vad::pop`] after consuming.
    pub fn front(&self) -> Option<VadSegment> {
        if self.is_empty() {
            return None;
        }
        unsafe {
            let seg: *const SpeechSegment = SherpaOnnxVoiceActivityDetectorFront(self.ptr);
            if seg.is_null() {
                return None;
            }
            let start = (*seg).start as i64;
            let n = (*seg).n as usize;
            let samples = if (*seg).samples.is_null() || n == 0 {
                Vec::new()
            } else {
                std::slice::from_raw_parts((*seg).samples, n).to_vec()
            };
            SherpaOnnxDestroySpeechSegment(seg);
            Some(VadSegment { start, samples })
        }
    }

    pub fn pop(&self) {
        unsafe { SherpaOnnxVoiceActivityDetectorPop(self.ptr) }
    }

    /// Force any buffered speech to flush as a final segment (utterance end).
    pub fn flush(&self) {
        unsafe { SherpaOnnxVoiceActivityDetectorFlush(self.ptr) }
    }

    /// Whether the VAD is inside speech right now (a segment is open and not
    /// yet closed by a pause).
    pub fn detected(&self) -> bool {
        unsafe { SherpaOnnxVoiceActivityDetectorDetected(self.ptr) != 0 }
    }

    /// Clear the VAD: its model state, queued segments, and its sample clock,
    /// which restarts at 0.
    pub fn reset(&self) {
        unsafe { SherpaOnnxVoiceActivityDetectorReset(self.ptr) }
    }
}

impl Drop for Vad {
    fn drop(&mut self) {
        unsafe { SherpaOnnxDestroyVoiceActivityDetector(self.ptr) }
    }
}

fn cstr(s: &str) -> CString {
    CString::new(s).unwrap_or_default()
}

/// The CTC blank token id from a sherpa `tokens.txt` (`<blk> N`). zipformer ships
/// `<blk> 0`, NeMo Conformer-CTC ships `<blk> 1024`; the offline factory keys the
/// model architecture (and the HL's blank handling) off this. Defaults to 0.
fn blank_id_from_tokens(path: &str) -> i32 {
    std::fs::read_to_string(path)
        .ok()
        .and_then(|s| {
            s.lines().find_map(|line| {
                let mut it = line.split_whitespace();
                match (it.next(), it.next()) {
                    (Some("<blk>"), Some(id)) => id.parse::<i32>().ok(),
                    _ => None,
                }
            })
        })
        .unwrap_or(0)
}

/// Normalize an offline FST decode's text. NeMo's blank (id 1024) renders as a
/// literal `<blk>` token interleaved between word pieces (the zipformer's blank=0
/// is suppressed by sherpa, so its text is already clean). Dropping the symbol and
/// collapsing whitespace rebuilds the command words — the BPE pieces carry their own
/// word-boundary marks, so no spaces are lost. Uppercased to match the zipformer
/// path's output (the closed grammar only emits command words).
fn clean_offline_text(text: String) -> String {
    text.replace("<blk>", "")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
        .to_uppercase()
}

#[cfg(test)]
mod offline_swap_verify {
    use super::*;
    use crate::find_onnx;

    // Locate the PCM data chunk of a canonical 16k/mono/PCM16 WAV and return it
    // as f32 in [-1, 1]. Scans for the "data" subchunk rather than assuming a
    // fixed 44-byte header (fixtures may carry LIST/fact chunks).
    fn read_wav_f32(path: &str) -> Vec<f32> {
        let bytes = std::fs::read(path).expect("read wav");
        let pos = bytes
            .windows(4)
            .position(|w| w == b"data")
            .expect("no data chunk");
        let data_start = pos + 8; // "data" + 4-byte size
        crate::pcm_to_f32(&bytes[data_start..])
    }

    /// Per-word bias mic-free verification, self-seed: per-word weights
    /// passed to the in-process GrammarBuilder change the decode at runtime. Seeds a
    /// closed union (no bundled HL.fst), decodes a known command unbiased, penalizes
    /// the decoded word(s) hard and asserts the decode changes, then proves dropping
    /// the bias restores it (the weight is a per-build property, not a sticky
    /// recognizer mutation). Run with:
    ///   BK_SWAP_MODEL="$HOME/Library/Application Support/BranchKitDev/artifacts/voice/sherpa-offline-nemo" \
    ///   BK_FIXTURES="$HOME/Library/Application Support/BranchKitDev/engine-fixtures" \
    ///   SHERPA_ONNX_LIB_DIR=stages/sherpa-commands/.sherpa-libs/<target-triple> \
    ///   cargo test -p sherpa-commands --release -- --ignored offline_grammar_bias --nocapture
    #[test]
    #[ignore]
    fn offline_grammar_bias_changes_decode() {
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        // Self-seed: no bundled HL.fst — construct empty and seed the union.
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer");
        let samples = fixture("copy_1.wav");

        // A union with acoustic neighbours, so a penalized word has somewhere to
        // fall (the collision the bias is meant to break).
        let union: Vec<String> = ["COPY", "PASTE", "UP", "DOWN", "LEFT", "RIGHT", "SNAP", "SCROLL"]
            .iter()
            .map(|s| s.to_string())
            .collect();

        // Unbiased in-process seed should decode the spoken command.
        recognizer.set_graph_from_words(&union, &[]);
        let unbiased = recognizer.decode(&samples).trim().to_string();
        eprintln!("unbiased in-process union ({} words): {unbiased:?}", union.len());
        assert!(!unbiased.is_empty(), "unbiased in-process decode was empty");

        // Penalize every word the unbiased decode emitted (+60, well past the
        // spike's ~30-40 flip threshold). Weights ride POSITIONALLY with `union`,
        // so they survive the internal case-fold. The decode must change.
        let penalized: std::collections::HashSet<&str> = unbiased.split_whitespace().collect();
        let weights: Vec<f32> =
            union.iter().map(|w| if penalized.contains(w.as_str()) { 60.0 } else { 0.0 }).collect();
        recognizer.set_graph_from_words(&union, &weights);
        let biased = recognizer.decode(&samples).trim().to_string();
        eprintln!("biased (penalize {penalized:?} by 60): {biased:?}");
        assert_ne!(
            biased, unbiased,
            "weighting had no effect — penalized grammar still produced the unbiased decode"
        );

        // Reversible: dropping the bias restores the unbiased decode, proving the
        // weight is a per-build property, not a sticky recognizer mutation.
        recognizer.set_graph_from_words(&union, &[]);
        let restored = recognizer.decode(&samples).trim().to_string();
        eprintln!("restored (bias dropped): {restored:?}");
        assert_eq!(restored, unbiased, "restore failed — dropping the bias did not return the decode");
    }

    /// Read a named fixture from the fixtures dir (`BK_FIXTURES`) as 16k mono
    /// f32. `just engine-tests` generates the clips with macOS TTS
    /// (tools/make_engine_fixtures.py), keeping only clips that decode to
    /// their own words, and runs these tests against them.
    fn fixture(name: &str) -> Vec<f32> {
        let dir = std::env::var("BK_FIXTURES")
            .expect("set BK_FIXTURES to the generated clips — run `just engine-tests`");
        read_wav_f32(&format!("{dir}/{name}"))
    }

    /// Behavioral validation of the LOAD-BEARING C++ grammar builder
    /// (`OfflineRecognizerCtcImpl::SetGraphFromWords` -> `GrammarBuilder::Build`,
    /// the runtime path that actually ships and does the live `--dynamic-grammar`
    /// rebuilds). Feeds the builder a word list and asserts the resulting closed
    /// grammar (a) ACCEPTS the listed words — each spoken command decodes to
    /// itself — and (b) REJECTS out-of-vocab speech — a word removed from the
    /// list becomes unproducible, since a closed FST grammar can only ever emit
    /// words it contains. Behavioral, not FST-byte-equality against the Python
    /// recipe: two correct builders can emit equivalent-but-not-identical graphs,
    /// so byte-equality would just re-introduce the dual-source coupling. Run:
    ///   BK_SWAP_MODEL="$HOME/Library/Application Support/BranchKitDev/artifacts/voice/sherpa-offline-nemo" \
    ///   BK_FIXTURES="$HOME/Library/Application Support/BranchKitDev/engine-fixtures" \
    ///   SHERPA_ONNX_LIB_DIR=stages/sherpa-commands/.sherpa-libs/<target-triple> \
    ///   cargo test -p sherpa-commands --release -- --ignored grammar_builder_closed_over_word_list --nocapture
    #[test]
    #[ignore]
    fn grammar_builder_closed_over_word_list() {
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        // Self-seed: no bundled HL.fst — construct empty and seed in-process.
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer");

        // (a) ACCEPTS: a grammar built in-process over these commands decodes each
        // spoken command to itself. This exercises the C++ GrammarBuilder
        // (word -> BPE -> L -> compose H -> determinize), not the baked HL.fst.
        let vocab: Vec<String> =
            ["COPY", "PASTE", "UP", "DOWN", "LEFT", "RIGHT"].iter().map(|s| s.to_string()).collect();
        recognizer.set_graph_from_words(&vocab, &[]);
        for (fx, expected) in [
            ("copy_1.wav", "COPY"),
            ("paste_1.wav", "PASTE"),
            ("up_1.wav", "UP"),
            ("down_1.wav", "DOWN"),
            ("left_1.wav", "LEFT"),
            ("right_1.wav", "RIGHT"),
        ] {
            let got = recognizer.decode(&fixture(fx)).trim().to_string();
            eprintln!("accept: {fx} -> {got:?} (want {expected:?})");
            assert_eq!(got, expected, "in-grammar word {expected:?} should be accepted");
        }

        // (b) REJECTS: rebuild WITHOUT copy/paste. A closed grammar can only emit
        // words it contains, so copy/paste speech must decode to in-grammar
        // neighbors (or empty) — the excluded words become unproducible. This is
        // the property the whole closed-grammar design rests on.
        let narrowed: Vec<String> =
            ["UP", "DOWN", "LEFT", "RIGHT"].iter().map(|s| s.to_string()).collect();
        let allowed: std::collections::HashSet<&str> = narrowed.iter().map(|s| s.as_str()).collect();
        recognizer.set_graph_from_words(&narrowed, &[]);
        for fx in ["copy_1.wav", "paste_1.wav"] {
            let got = recognizer.decode(&fixture(fx)).trim().to_string();
            eprintln!("reject: {fx} -> {got:?} (grammar {narrowed:?})");
            for w in got.split_whitespace() {
                assert!(
                    allowed.contains(w),
                    "closed grammar emitted out-of-vocab word {w:?} (full decode {got:?})"
                );
            }
            assert!(
                !got.contains("COPY") && !got.contains("PASTE"),
                "excluded word leaked into the decode: {got:?}"
            );
        }
    }

    /// The build half runs on another thread WHILE this one
    /// decodes, the built graph changes nothing until it is installed, and
    /// the install is a pointer swap. Same model/fixtures as the tests above;
    /// `just engine-tests` runs it.
    #[test]
    #[ignore]
    fn graph_built_on_another_thread_installs_between_decodes() {
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        let recognizer = std::sync::Arc::new(
            OfflineRecognizer::create(
                &onnx.to_string_lossy(),
                &dir.join("tokens.txt").to_string_lossy(),
                "",
                3000,
            )
            .expect("create offline recognizer"),
        );
        let narrow: Vec<String> =
            ["UP", "DOWN", "LEFT", "RIGHT"].iter().map(|s| s.to_string()).collect();
        recognizer.set_graph_from_words(&narrow, &[]);

        let wide: Vec<String> = ["COPY", "PASTE", "UP", "DOWN", "LEFT", "RIGHT"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        let builder = std::sync::Arc::clone(&recognizer);
        let build = std::thread::spawn(move || builder.build_graph_from_words(&wide, &[]));
        // Decode on this thread while the other builds.
        let left = fixture("left_1.wav");
        for _ in 0..5 {
            assert_eq!(recognizer.decode(&left).trim(), "LEFT");
        }
        let graph = build.join().expect("build thread").expect("a CTC FST recognizer builds a graph");

        // Built, not installed: the narrow grammar still rules.
        let copy = fixture("copy_1.wav");
        let before = recognizer.decode(&copy);
        assert!(!before.contains("COPY"), "an uninstalled graph changed the decode: {before:?}");

        let t0 = std::time::Instant::now();
        recognizer.install_graph(graph);
        let install = t0.elapsed();
        eprintln!("install: {:.3}ms", install.as_secs_f64() * 1000.0);
        assert!(install.as_millis() < 5, "install is a pointer swap, took {install:?}");
        assert_eq!(recognizer.decode(&copy).trim(), "COPY");
    }

    /// With the switch on, every onnxruntime worker the
    /// recognizer's session creates reads back at interactive priority from
    /// inside the thread (macOS: `pthread_get_qos_class_np`; ps cannot show
    /// QoS), and so does the calling thread after the raise.
    #[test]
    #[ignore]
    #[cfg(any(target_os = "macos", windows))]
    fn inference_threads_run_at_interactive_priority() {
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        assert!(enable_interactive_inference_threads(), "this OS supports the raise");
        let (before, _) = interactive_inference_thread_stats();
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer");
        // Workers report from inside themselves, asynchronously.
        let mut stats = interactive_inference_thread_stats();
        for _ in 0..100 {
            if stats.0 > before {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(10));
            stats = interactive_inference_thread_stats();
        }
        let (created, raised) = stats;
        eprintln!("onnxruntime workers: {created} created, {raised} raised");
        assert!(created > before, "the session created no worker through the hook");
        assert_eq!(raised, created, "a worker came up below interactive priority");
        assert!(raise_current_thread_to_interactive());
        drop(recognizer);
    }

    /// Step 2 regression: the empty-graph SELF-SEED path. Constructs the offline
    /// recognizer with NO graph (the self-seed startup, which yields the greedy
    /// open-vocab decoder) and asserts the first set_graph_from_words PROMOTES it
    /// to the closed-grammar FST decoder. Before the C++ promote patch this leaked
    /// out-of-vocab words (the greedy decoder ignored the seed — see the Step-0
    /// finding); after it, the grammar is genuinely closed. Run:
    ///   BK_SWAP_MODEL="$HOME/Library/Application Support/BranchKitDev/artifacts/voice/sherpa-offline-nemo" \
    ///   BK_FIXTURES="$HOME/Library/Application Support/BranchKitDev/engine-fixtures" \
    ///   SHERPA_ONNX_LIB_DIR=stages/sherpa-commands/.sherpa-libs/<target-triple> \
    ///   cargo test -p sherpa-commands --release -- --ignored empty_graph_self_seed_promotes --nocapture
    #[test]
    #[ignore]
    fn empty_graph_self_seed_promotes() {
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");

        // Construct with an EMPTY graph — the self-seed startup (greedy decoder).
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer with empty graph");

        // First seed EXCLUDES copy/paste. The promote patch makes this a genuine
        // closed grammar, so those words must be unproducible.
        let narrowed: Vec<String> =
            ["UP", "DOWN", "LEFT", "RIGHT"].iter().map(|s| s.to_string()).collect();
        let allowed: std::collections::HashSet<&str> = narrowed.iter().map(|s| s.as_str()).collect();
        recognizer.set_graph_from_words(&narrowed, &[]);
        for fx in ["copy_1.wav", "paste_1.wav"] {
            let got = recognizer.decode(&fixture(fx)).trim().to_string();
            eprintln!("self-seed reject: {fx} -> {got:?} (grammar {narrowed:?})");
            for w in got.split_whitespace() {
                assert!(
                    allowed.contains(w),
                    "self-seeded grammar emitted out-of-vocab word {w:?} (full decode {got:?}) \
                     — promote patch not in effect?"
                );
            }
            assert!(
                !got.contains("COPY") && !got.contains("PASTE"),
                "excluded word leaked after self-seed: {got:?} — promote patch not in effect?"
            );
        }

        // Re-seed INCLUDING copy — the promoted FST decoder now accepts it.
        let wide: Vec<String> =
            ["COPY", "PASTE", "UP", "DOWN", "LEFT", "RIGHT"].iter().map(|s| s.to_string()).collect();
        recognizer.set_graph_from_words(&wide, &[]);
        let got = recognizer.decode(&fixture("copy_1.wav")).trim().to_string();
        eprintln!("self-seed accept: copy_1.wav -> {got:?}");
        assert_eq!(got, "COPY", "self-seeded grammar should accept COPY after a wide re-seed");
    }

    /// The structured-grammar build stays fast at the live grammar's size: a
    /// trim that expanded every dead end of HL∘G took ~12 s per command-set
    /// change, and a hold meanwhile waited for it. Needs only
    /// the model:
    ///   BK_SWAP_MODEL=... SHERPA_ONNX_LIB_DIR=... cargo test -p sherpa-commands \
    ///   --release -- --ignored a_live_size_structured_grammar --nocapture
    #[test]
    #[ignore]
    fn a_live_size_structured_grammar_builds_in_well_under_two_seconds() {
        use branchkit_stage_sdk::grammar_dag::{GrammarDagWire, WireArc};
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer");

        // The live command grammar's shape on 2026-10-20: ~1.9k words of
        // 1-5 BPE pieces, a DAG of ~5.4k states that each admit only a few of
        // them. 1900 made-up words of two to five syllables, 5400 two-word
        // commands, each first word followed by 8 second words.
        let syllables = [
            "ba", "kor", "ri", "mun", "te", "sal", "lo", "nix", "da", "fen", "gu", "pi",
        ];
        let words: Vec<String> = (0..1900usize)
            .map(|i| {
                let len = 2 + i % 4;
                let mut n = i;
                let mut w = String::new();
                for _ in 0..len {
                    w.push_str(syllables[n % 12]);
                    n = n / 12 + 7;
                }
                w.to_uppercase()
            })
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter()
            .collect();
        let mut arcs = Vec::new();
        for s in 1..=5400usize {
            let first = &words[s % words.len()];
            arcs.push(WireArc {
                from: 0,
                word: first.clone(),
                to: s,
                weight: None,
            });
            for k in 0..8 {
                arcs.push(WireArc {
                    from: s,
                    word: words[(s * 7 + k * 131) % words.len()].clone(),
                    to: 0,
                    weight: None,
                });
            }
        }
        let dag = GrammarDagWire {
            num_states: 5401,
            start: 0,
            arcs,
            open_states: vec![],
            dropped_alts: 0,
        };
        // Before the lookahead trim this expanded every dead-end prefix of
        // the composition: ~12 s at the live size.
        let t0 = std::time::Instant::now();
        let graph = recognizer.build_graph_from_dag(&dag, &words);
        let took = t0.elapsed();
        eprintln!("live-size structured grammar built in {took:?}");
        assert!(graph.is_some(), "the grammar builds");
        assert!(
            took < std::time::Duration::from_secs(2),
            "a live-size structured grammar took {took:?} to build"
        );
    }

    /// Command-grammar compiler D2: the structured `H ∘ L ∘ G` path. Proves the
    /// pattern-DAG constrains word *sequences*, not just the word set — the exact
    /// lever behind the live "scroll up -> scroll eight" mishear fix. Three parts:
    /// (a) a codeword DAG decodes each command to itself (BuildFromDag yields a
    /// working closed decoder); (b) the SAME word set, gated so "up" is reachable
    /// only after "scroll", makes a bare-"up" clip structurally unable to decode
    /// to "UP" — whereas the FLAT union over those words decodes it to "UP"; and
    /// (c) the real "scroll up" command still decodes through the gated DAG. Run:
    ///   BK_SWAP_MODEL="$HOME/Library/Application Support/BranchKitDev/artifacts/voice/sherpa-offline-nemo" \
    ///   BK_FIXTURES="$HOME/Library/Application Support/BranchKitDev/engine-fixtures" \
    ///   SHERPA_ONNX_LIB_DIR=stages/sherpa-commands/.sherpa-libs/<target-triple> \
    ///   cargo test -p sherpa-commands --release -- --ignored dag_grammar_gates_word_by_context --nocapture
    #[test]
    #[ignore]
    fn dag_grammar_gates_word_by_context() {
        use branchkit_stage_sdk::grammar_dag::{GrammarDagWire, WireArc};
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer");

        let arc = |from: usize, word: &str, to: usize| WireArc {
            from,
            word: word.to_string(),
            to,
            weight: None,
        };

        // (a) ACCEPTS: codewords as loop arcs on the start state decode to
        // themselves through the DAG path (BuildFromDag -> working closed FST).
        let codewords = GrammarDagWire {
            num_states: 1,
            start: 0,
            arcs: vec![
                arc(0, "COPY", 0),
                arc(0, "PASTE", 0),
                arc(0, "UP", 0),
                arc(0, "DOWN", 0),
                arc(0, "LEFT", 0),
                arc(0, "RIGHT", 0),
            ],
            open_states: vec![],
            dropped_alts: 0,
        };
        recognizer.set_graph_from_dag(&codewords, &[]);
        for (fx, want) in [("up_1.wav", "UP"), ("down_1.wav", "DOWN"), ("copy_1.wav", "COPY")] {
            let got = recognizer.decode(&fixture(fx)).trim().to_string();
            eprintln!("dag accept: {fx} -> {got:?} (want {want:?})");
            assert_eq!(got, want, "DAG codeword {want:?} must decode to itself");
        }

        // (b) THE WIN: gate "up"/"down" behind "scroll" so they are NOT leading
        // words. States: 0 = start, 1 = after "scroll".
        //   0 -scroll-> 1 ; 1 -up-> 0 ("scroll up") ; 1 -down-> 0 ("scroll down")
        let gated = GrammarDagWire {
            num_states: 2,
            start: 0,
            arcs: vec![arc(0, "SCROLL", 1), arc(1, "UP", 0), arc(1, "DOWN", 0)],
            open_states: vec![],
            dropped_alts: 0,
        };

        // Same word set as a FLAT union: "up" can lead, so a bare-"up" clip -> "UP".
        recognizer.set_graph_from_words(
            &["SCROLL".to_string(), "UP".to_string(), "DOWN".to_string()],
            &[],
        );
        let flat_up = recognizer.decode(&fixture("up_1.wav")).trim().to_string();
        eprintln!("flat union: up_1.wav -> {flat_up:?}");
        assert_eq!(flat_up, "UP", "under a flat union a bare 'up' clip decodes to UP");

        // DAG: "up" is gated behind "scroll" -> a bare-"up" clip CANNOT be "UP".
        recognizer.set_graph_from_dag(&gated, &[]);
        let dag_up = recognizer.decode(&fixture("up_1.wav")).trim().to_string();
        eprintln!("dag gated: up_1.wav -> {dag_up:?}");
        assert_ne!(
            dag_up, "UP",
            "DAG gates 'up' behind 'scroll' — bare 'up' must be structurally unproducible"
        );
        for w in dag_up.split_whitespace() {
            assert!(
                ["SCROLL", "UP", "DOWN"].contains(&w),
                "DAG emitted out-of-grammar word {w:?} (decode {dag_up:?})"
            );
        }

        // (c) The real command still decodes correctly through the gated DAG.
        let scroll_up = recognizer.decode(&fixture("scroll_up_1.wav")).trim().to_string();
        eprintln!("dag command: scroll_up_1.wav -> {scroll_up:?}");
        assert_eq!(scroll_up, "SCROLL UP", "the gated command 'scroll up' must still decode");
    }

    /// D2 open-tail alphabet: an open state (a free-text / dependent-capture tail,
    /// e.g. a hint codeword's suffix) must be able to decode a word that exists
    /// only in the recognition UNION, not as a DAG arc — otherwise dynamic hint
    /// suffixes (pushed via the union, never enumerated into the DAG) would stop
    /// decoding once the structured grammar is live. `cap` leads to an open state;
    /// `red` is supplied only via the union. Run:
    ///   BK_SWAP_MODEL=... BK_FIXTURES=... SHERPA_ONNX_LIB_DIR=... \
    ///   cargo test -p sherpa-commands --release -- --ignored dag_open_tail_decodes_union_word --nocapture
    #[test]
    #[ignore]
    fn dag_open_tail_decodes_union_word() {
        use branchkit_stage_sdk::grammar_dag::{GrammarDagWire, WireArc};
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer");

        // 0 -cap-> 1 ; state 1 is OPEN (a dependent-suffix tail). "red" is NOT a
        // DAG arc — it only exists in the union.
        let dag = GrammarDagWire {
            num_states: 2,
            start: 0,
            arcs: vec![WireArc { from: 0, word: "CAP".to_string(), to: 1, weight: None }],
            open_states: vec![branchkit_stage_sdk::grammar_dag::OpenState { state: 1, alphabet: None }],
            dropped_alts: 0,
        };

        // With the union supplying "RED", the open tail decodes "CAP RED".
        recognizer.set_graph_from_dag(&dag, &["CAP".to_string(), "RED".to_string()]);
        let with_union = recognizer.decode(&fixture("cap_red_1.wav")).trim().to_string();
        eprintln!("open tail (union has RED): cap_red_1.wav -> {with_union:?}");
        assert_eq!(
            with_union, "CAP RED",
            "the open-tail self-loop must cover union words that are not DAG arcs"
        );

        // Without the union (open-tail alphabet = DAG words = {CAP}), "RED" is
        // unreachable, so the tail cannot produce it.
        recognizer.set_graph_from_dag(&dag, &[]);
        let no_union = recognizer.decode(&fixture("cap_red_1.wav")).trim().to_string();
        eprintln!("open tail (no union): cap_red_1.wav -> {no_union:?}");
        assert_ne!(
            no_union, "CAP RED",
            "without the union alphabet the open tail cannot reach a non-DAG word"
        );
    }

    /// Phase 3 narrowing: a per-open-state bounded alphabet shrinks the tail
    /// self-loop below the full union. Even with "RED" supplied in the union, an
    /// open state bounded to {CAP} cannot decode "RED" at the tail — proving C++
    /// loops over `open_alphabets`, not the whole union. The mirror of
    /// `dag_open_tail_decodes_union_word` (where the same union yields "CAP RED").
    #[test]
    #[ignore]
    fn dag_open_tail_bounded_alphabet_excludes_union_word() {
        use branchkit_stage_sdk::grammar_dag::{GrammarDagWire, OpenState, WireArc};
        let Ok(model_dir) = std::env::var("BK_SWAP_MODEL") else {
            eprintln!("skip: set BK_SWAP_MODEL to the offline model dir");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("model.onnx in BK_SWAP_MODEL");
        let recognizer = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("create offline recognizer");

        // 0 -cap-> 1 ; state 1 OPEN but bounded to {CAP}. "RED" is in the union
        // (and decodes as the tail when unbounded, per the sibling test) but is
        // excluded from this state's alphabet.
        let bounded = GrammarDagWire {
            num_states: 2,
            start: 0,
            arcs: vec![WireArc { from: 0, word: "CAP".to_string(), to: 1, weight: None }],
            open_states: vec![OpenState { state: 1, alphabet: Some(vec!["CAP".to_string()]) }],
            dropped_alts: 0,
        };
        recognizer.set_graph_from_dag(&bounded, &["CAP".to_string(), "RED".to_string()]);
        let got = recognizer.decode(&fixture("cap_red_1.wav")).trim().to_string();
        eprintln!("bounded tail {{CAP}} (union has RED): cap_red_1.wav -> {got:?}");
        assert!(
            !got.split_whitespace().any(|w| w == "RED"),
            "a bounded open-tail alphabet must exclude a union word outside the set (decode {got:?})"
        );
    }

    /// Reproduce the live "commands go empty under D2" bug offline against the
    /// EXACT grammar the running platform produced. Load a dump written by
    /// `BRANCHKIT_DUMP_GRAMMAR` (`{grammar_dag, union}`) and decode a set of real
    /// clips under BOTH the flat union (known-good baseline) and the structured
    /// DAG, printing the two side by side to localize where the DAG diverges.
    /// Diagnostic (prints, no assert). Run:
    ///   BK_SWAP_MODEL=... SHERPA_ONNX_LIB_DIR=... \
    ///   BK_GRAMMAR_DUMP=/tmp/bk_grammar_dump.json \
    ///   BK_CLIPS="copy=/abs/copy_1.wav,air air=/abs/seg_00000.wav" \
    ///   cargo test -p sherpa-commands --release -- --ignored dag_repro_from_live_dump --nocapture
    #[test]
    #[ignore]
    fn dag_repro_from_live_dump() {
        use branchkit_stage_sdk::grammar_dag::GrammarDagWire;
        let (Ok(model_dir), Ok(dump_path), Ok(clips_env)) = (
            std::env::var("BK_SWAP_MODEL"),
            std::env::var("BK_GRAMMAR_DUMP"),
            std::env::var("BK_CLIPS"),
        ) else {
            eprintln!("skip: set BK_SWAP_MODEL, BK_GRAMMAR_DUMP, BK_CLIPS");
            return;
        };
        #[derive(serde::Deserialize)]
        struct Dump {
            grammar_dag: GrammarDagWire,
            union: Vec<String>,
        }
        let dump: Dump = serde_json::from_str(
            &std::fs::read_to_string(&dump_path).expect("read dump"),
        )
        .expect("parse dump");
        eprintln!(
            "DAG: {} states, {} arcs, {} open, {} union words",
            dump.grammar_dag.num_states,
            dump.grammar_dag.arcs.len(),
            dump.grammar_dag.open_states.len(),
            dump.union.len()
        );

        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("onnx");
        let rec = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("recognizer");

        for spec in clips_env.split(',') {
            let (label, path) = spec.split_once('=').unwrap_or(("clip", spec));
            let samples = read_wav_f32(path.trim());
            rec.set_graph_from_words(&dump.union, &[]);
            let flat = rec.decode(&samples).trim().to_string();
            rec.set_graph_from_dag(&dump.grammar_dag, &dump.union);
            let dagd = rec.decode(&samples).trim().to_string();
            let flag = if flat != dagd { "  <-- DIVERGES" } else { "" };
            eprintln!("{:16} flat={:?}  dag={:?}{}", label.trim(), flat, dagd, flag);
        }
    }
}

#[cfg(test)]
mod open_vocab_probe {
    use super::*;
    use crate::find_onnx;

    /// Greedy open-vocab decode of clips — what does the model hear WITHOUT any
    /// grammar? THE triage tool for "segment decoded empty": a closed grammar
    /// decodes off-grammar speech to "" (blank beats every path), which is
    /// indistinguishable in the logs from silence; the greedy readout names the
    /// words that were actually spoken (2026-07-02: six "empty" pair clips
    /// turned out to be "AIR AIR" — a retired codeword deck, not a DAG gap). Run:
    ///   BK_SWAP_MODEL=... BK_CLIPS="label=/abs.wav,..." SHERPA_ONNX_LIB_DIR=... \
    ///   cargo test -p sherpa-commands --release -- --ignored greedy_probe --nocapture
    #[test]
    #[ignore]
    fn greedy_probe() {
        let (Ok(model_dir), Ok(clips_env)) =
            (std::env::var("BK_SWAP_MODEL"), std::env::var("BK_CLIPS"))
        else {
            eprintln!("skip: set BK_SWAP_MODEL, BK_CLIPS");
            return;
        };
        let dir = std::path::Path::new(&model_dir);
        let onnx = find_onnx(dir).expect("onnx");
        // Empty graph + never seeded -> greedy open-vocab decoder.
        let rec = OfflineRecognizer::create(
            &onnx.to_string_lossy(),
            &dir.join("tokens.txt").to_string_lossy(),
            "",
            3000,
        )
        .expect("recognizer");
        for spec in clips_env.split(',') {
            let (label, path) = spec.split_once('=').unwrap_or(("clip", spec));
            let bytes = std::fs::read(path.trim()).expect("read wav");
            let pos = bytes.windows(4).position(|w| w == b"data").expect("no data chunk");
            let samples = crate::pcm_to_f32(&bytes[pos + 8..]);
            let got = rec.decode(&samples).trim().to_string();
            eprintln!("{:8} greedy={:?}", label.trim(), got);
        }
    }
}
