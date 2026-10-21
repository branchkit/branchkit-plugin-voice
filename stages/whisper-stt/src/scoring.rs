//! Per-word scores and audio cut-off for a batch transcript, kept free of the
//! engine so they are tested without a model.
//!
//! The scores follow the stage contract on `Transcript::word_scores`: sign is
//! the contract, magnitude the engine's own scale, positive means the audio
//! supported the word. This stage's scale is the word's mean natural-log
//! token probability MINUS ln(0.5): positive when the decoder gave the word's
//! tokens better than even odds, negative when it did not. The macOS
//! WhisperKit stage computes the same quantity, so both platforms' consumers
//! read one scale.

/// ln(0.5): the even-odds line a word's mean token log-probability is
/// measured against.
const EVEN_ODDS_LN: f32 = -std::f32::consts::LN_2;

/// One decoded token: its text as the tokenizer renders it (a leading space
/// marks a word start) and the log-probability of choosing it.
#[derive(Debug, Clone, PartialEq)]
pub struct ScoredToken {
    pub text: String,
    pub logprob: f32,
}

/// The per-word scores for `text`, aligned 1:1 with its whitespace-split
/// words, or `None` when the tokens cannot be aligned to those words (a
/// consumer is better served by no scores than by scores on the wrong words).
///
/// A token whose text begins with whitespace starts a word; any other token
/// (a word's later piece, or punctuation) belongs to the word before it,
/// which is how whitespace splitting groups "Kit." with "Branch".
pub fn word_scores(tokens: &[ScoredToken], text: &str) -> Option<Vec<f32>> {
    let mut words: Vec<(f32, u32)> = Vec::new();
    for t in tokens {
        if t.text.trim().is_empty() {
            continue;
        }
        let starts_word = t.text.starts_with(char::is_whitespace) || words.is_empty();
        if starts_word {
            words.push((t.logprob, 1));
        } else if let Some(last) = words.last_mut() {
            last.0 += t.logprob;
            last.1 += 1;
        }
    }
    if words.len() != text.split_whitespace().count() || words.is_empty() {
        return None;
    }
    Some(
        words
            .into_iter()
            .map(|(sum, n)| sum / n as f32 - EVEN_ODDS_LN)
            .collect(),
    )
}

/// The transcript-level score the contract asks for: the mean of the word
/// scores when there are any, else the mean token log-probability on the
/// same scale (its own scalar).
pub fn confidence(tokens: &[ScoredToken], word_scores: Option<&[f32]>) -> Option<f32> {
    if let Some(ws) = word_scores.filter(|w| !w.is_empty()) {
        return Some(ws.iter().sum::<f32>() / ws.len() as f32);
    }
    let counted: Vec<f32> = tokens
        .iter()
        .filter(|t| !t.text.trim().is_empty())
        .map(|t| t.logprob)
        .collect();
    if counted.is_empty() {
        return None;
    }
    Some(counted.iter().sum::<f32>() / counted.len() as f32 - EVEN_ODDS_LN)
}

/// How many buffered samples to keep for a stop with `cutoff_ms`, the same
/// rule as the macOS stage's: find the first chunk stamped at or after the
/// cut-off and step back from its end by the milliseconds between the two
/// (16 samples each at 16 kHz). `marks` holds, per chunk in arrival order,
/// (samples buffered once it was appended, its shared-clock timestamp). No
/// cut-off, or one past every chunk, keeps everything.
pub fn samples_before_cutoff(
    marks: &[(usize, u64)],
    total: usize,
    cutoff_ms: Option<u64>,
) -> usize {
    let Some(cutoff) = cutoff_ms else {
        return total;
    };
    let Some(&(end, ts)) = marks.iter().find(|(_, ts)| *ts >= cutoff) else {
        return total;
    };
    end.saturating_sub((ts - cutoff) as usize * 16).min(total)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tok(text: &str, p: f32) -> ScoredToken {
        ScoredToken {
            text: text.into(),
            logprob: p.ln(),
        }
    }

    #[test]
    fn words_group_their_pieces_and_trailing_punctuation() {
        let tokens = [
            tok(" Open", 0.9),
            tok(" Branch", 0.8),
            tok("Kit", 0.1),
            tok(".", 0.99),
        ];
        let ws = word_scores(&tokens, "Open BranchKit.").unwrap();
        assert_eq!(ws.len(), 2);
        assert!(ws[0] > 0.0, "a confident word scores positive: {ws:?}");
        // ln(0.8)+ln(0.1)+ln(0.99) over 3 is below ln(0.5).
        assert!(
            ws[1] < 0.0,
            "a doubtful piece pulls its word negative: {ws:?}"
        );
    }

    #[test]
    fn a_word_at_exactly_even_odds_scores_zero() {
        let ws = word_scores(&[tok(" yes", 0.5)], "yes").unwrap();
        assert!(ws[0].abs() < 1e-6);
    }

    #[test]
    fn misaligned_tokens_give_no_scores() {
        // Two token words against three text words: refuse rather than guess.
        let tokens = [tok(" hello", 0.9), tok(" world", 0.9)];
        assert_eq!(word_scores(&tokens, "hello big world"), None);
        assert_eq!(word_scores(&[], "hello"), None);
    }

    #[test]
    fn the_first_token_starts_a_word_without_a_space() {
        let ws = word_scores(&[tok("Hi", 0.9), tok(" there", 0.9)], "Hi there").unwrap();
        assert_eq!(ws.len(), 2);
    }

    #[test]
    fn confidence_is_the_mean_of_word_scores_else_of_tokens() {
        assert_eq!(confidence(&[], Some(&[1.0, 3.0])), Some(2.0));
        let c = confidence(&[tok(" a", 0.5), tok(" b", 0.5)], None).unwrap();
        assert!(c.abs() < 1e-6);
        assert_eq!(confidence(&[], None), None);
    }

    #[test]
    fn cutoff_steps_back_from_the_first_chunk_at_or_after_it() {
        // Three 20 ms chunks of 320 samples, stamped at their ends.
        let marks = [(320, 100), (640, 120), (960, 140)];
        assert_eq!(samples_before_cutoff(&marks, 960, None), 960);
        assert_eq!(samples_before_cutoff(&marks, 960, Some(120)), 640);
        // 5 ms before the second chunk's end: 80 samples back from 640.
        assert_eq!(samples_before_cutoff(&marks, 960, Some(115)), 560);
        // Before any audio: nothing is kept.
        assert_eq!(samples_before_cutoff(&marks, 960, Some(50)), 0);
        assert_eq!(samples_before_cutoff(&marks, 960, Some(10_000)), 960);
    }
}
