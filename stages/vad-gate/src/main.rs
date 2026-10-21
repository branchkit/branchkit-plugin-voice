use branchkit_stage_sdk::events::{
    AudioChunk, AudioFormat, AudioStart, AudioStop, Capability, event_type, new_session_id,
};
use branchkit_stage_sdk::stage::{
    self, AudioConsumer, AudioCtx, Chunk, CreditPolicy, Flow, InitialGrant, Result,
};
use branchkit_stage_sdk::stage_log;
use voice_activity_detector::VoiceActivityDetector;

const SAMPLE_RATE: usize = 16000;
const CHUNK_SIZE: usize = 512; // 32ms at 16kHz — required by Silero VAD
const SPEECH_THRESHOLD: f32 = 0.35;
const SPEECH_PAD_FRAMES: u32 = 2; // ~64ms of speech before we trigger
const SILENCE_PAD_FRAMES: u32 = 15; // ~480ms of silence before we end

// Wide window + wide cadence — this stage buffers freely. The counter
// deliberately survives session boundaries (audio_start does not reset it),
// which is what `InitialGrant::OnStart` means.
const CREDIT: CreditPolicy = CreditPolicy {
    initial: 32,
    every: 8,
    grant: 8,
    when: InitialGrant::OnStart,
};

#[tokio::main(flavor = "current_thread")]
async fn main() {
    stage::run(run()).await
}

async fn run() -> Result {
    let vad = VoiceActivityDetector::builder()
        .sample_rate(SAMPLE_RATE as i64)
        .chunk_size(CHUNK_SIZE)
        .build()
        .map_err(|e| format!("failed to build VAD: {e}"))?;

    let cap = Capability::new("vad_gate", "silero")
        .audio_format(AudioFormat::PCM_16K_MONO)
        .persistent()
        .emits(["audio_start", "audio_chunk", "audio_stop", "flow_credit"]);

    let mut gate = Gate {
        vad,
        in_speech: false,
        speech_count: 0,
        silence_count: 0,
        current_session: None,
        pcm_buffer: Vec::with_capacity(CHUNK_SIZE * 2),
        onset_buffer: Vec::new(),
        output_timestamp_ms: 0,
        peak_probability: 0.0,
    };

    stage::serve_audio_consumer(cap, CREDIT, &mut gate).await
}

struct Gate {
    vad: VoiceActivityDetector,
    in_speech: bool,
    speech_count: u32,
    silence_count: u32,
    current_session: Option<String>,
    pcm_buffer: Vec<i16>,
    // Buffer frames during speech onset detection so they aren't lost
    onset_buffer: Vec<Vec<i16>>,
    output_timestamp_ms: u64,
    peak_probability: f32,
}

impl Gate {
    /// Emit our own downstream `audio_stop` and clear session state.
    async fn end_session(&mut self, ctx: &mut AudioCtx, reason: &str) -> Result {
        if let Some(session_id) = self.current_session.take() {
            stage_log::info(reason);
            stage_log::clear_session();
            ctx.emit(
                event_type::AUDIO_STOP,
                &AudioStop {
                    session_id,
                    cutoff_ms: None,
                },
            )
            .await?;
        }
        Ok(())
    }

    /// Forward one VAD-sized frame downstream and advance the output clock.
    async fn forward(&mut self, ctx: &mut AudioCtx, samples: &[i16]) -> Result {
        let Some(session_id) = self.current_session.clone() else {
            return Ok(());
        };
        let payload: Vec<u8> = samples.iter().flat_map(|s| s.to_le_bytes()).collect();
        ctx.emit_raw(
            event_type::AUDIO_CHUNK,
            &AudioChunk {
                session_id,
                timestamp_ms: self.output_timestamp_ms,
            },
            &payload,
        )
        .await?;
        self.output_timestamp_ms += (CHUNK_SIZE as u64 * 1000) / SAMPLE_RATE as u64;
        Ok(())
    }
}

impl AudioConsumer for Gate {
    /// Upstream started a session — reset our state.
    async fn on_audio_start(&mut self, _ev: AudioStart, _ctx: &mut AudioCtx) -> Result {
        self.speech_count = 0;
        self.silence_count = 0;
        self.in_speech = false;
        self.pcm_buffer.clear();
        self.onset_buffer.clear();
        Ok(())
    }

    async fn on_audio_chunk(
        &mut self,
        _ev: AudioChunk,
        payload: &[u8],
        ctx: &mut AudioCtx,
    ) -> Result<Chunk> {
        // Decode i16 PCM from payload
        let samples: Vec<i16> = payload
            .chunks_exact(2)
            .map(|b| i16::from_le_bytes([b[0], b[1]]))
            .collect();
        self.pcm_buffer.extend_from_slice(&samples);

        // Process in VAD-sized chunks (512 samples)
        while self.pcm_buffer.len() >= CHUNK_SIZE {
            let raw_samples: Vec<i16> = self.pcm_buffer.drain(..CHUNK_SIZE).collect();
            let vad_frame: Vec<f32> = raw_samples.iter().map(|&s| s as f32 / 32768.0).collect();

            let probability = self.vad.predict(vad_frame);

            let mut already_forwarded = false;

            if probability > self.peak_probability {
                self.peak_probability = probability;
            }

            if probability >= SPEECH_THRESHOLD {
                self.speech_count += 1;
                self.silence_count = 0;

                if !self.in_speech {
                    self.onset_buffer.push(raw_samples.clone());

                    if self.speech_count >= SPEECH_PAD_FRAMES {
                        self.in_speech = true;
                        let session_id = new_session_id();
                        stage_log::set_session(&session_id);
                        stage_log::info(&format!("speech start peak={:.2}", self.peak_probability));
                        self.output_timestamp_ms = 0;
                        self.peak_probability = 0.0;

                        ctx.emit(
                            event_type::AUDIO_START,
                            &AudioStart {
                                session_id: session_id.clone(),
                                format: AudioFormat::PCM_16K_MONO,
                            },
                        )
                        .await?;
                        self.current_session = Some(session_id);

                        // Flush buffered onset frames (includes current frame)
                        for buffered in std::mem::take(&mut self.onset_buffer) {
                            self.forward(ctx, &buffered).await?;
                        }
                        already_forwarded = true;
                    }
                }
            } else {
                self.silence_count += 1;
                if self.speech_count > 0 && !self.in_speech {
                    stage_log::info(&format!(
                        "near-miss: {} speech frames (need {SPEECH_PAD_FRAMES}) peak={:.2} prob={probability:.2}",
                        self.speech_count, self.peak_probability
                    ));
                }
                self.speech_count = 0;
                self.onset_buffer.clear();
                if !self.in_speech && self.silence_count >= SILENCE_PAD_FRAMES {
                    self.peak_probability = 0.0;
                }

                if self.in_speech && self.silence_count >= SILENCE_PAD_FRAMES {
                    self.in_speech = false;
                    self.peak_probability = 0.0;
                    self.end_session(ctx, "speech end").await?;
                }
            }

            // Forward audio when in speech (skip if already sent via onset flush)
            if self.in_speech && !already_forwarded {
                self.forward(ctx, &raw_samples).await?;
            }
        }

        Ok(Chunk::Counted)
    }

    /// Upstream ended — if we're in speech, close our session.
    async fn on_audio_stop(&mut self, _ev: AudioStop, ctx: &mut AudioCtx) -> Result<Flow> {
        self.end_session(ctx, "speech end (upstream stop)").await?;
        self.in_speech = false;
        Ok(Flow::Continue)
    }
}
