//! llama.cpp's and ggml's log lines, sent through the stage log.
//!
//! Left alone, both libraries write every line to stderr, where the platform
//! records it at info. On macOS, starting the backend already opens the GPU
//! device and writes some 35 lines about it, so the route has to be in place
//! before the backend starts, not after. Errors become warnings: the stage itself says
//! what a failure meant (a model that does not load, a GPU that does not
//! start). Everything else goes out at debug or below, so a model load is
//! quiet at the default level and every line is still there for a stage
//! whose log level is lowered.

use std::ffi::{CStr, c_char, c_void};
use std::sync::Mutex;

use branchkit_stage_sdk::stage_log;
use llama_cpp_sys_2::{
    GGML_LOG_LEVEL_CONT, GGML_LOG_LEVEL_DEBUG, GGML_LOG_LEVEL_ERROR, ggml_log_level,
};

/// Send llama.cpp's and ggml's logs to the stage log from now on. Call it
/// before any other llama.cpp function: llama.cpp hands the same callback to
/// ggml.
pub fn route() {
    // SAFETY: llama_log_set only stores the callback and its user data; the
    // callback is a plain function that lives as long as the process.
    unsafe { llama_cpp_sys_2::llama_log_set(Some(on_log), std::ptr::null_mut()) }
}

static LINES: Mutex<Lines> = Mutex::new(Lines::new());

unsafe extern "C" fn on_log(level: ggml_log_level, text: *const c_char, _user: *mut c_void) {
    if text.is_null() {
        return;
    }
    // SAFETY: llama.cpp passes a NUL-terminated string that outlives the call.
    let text = unsafe { CStr::from_ptr(text) }.to_string_lossy();
    // A panic must not unwind into C; a line that cannot be written is lost.
    let _ = std::panic::catch_unwind(|| {
        let done = LINES
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .push(Piece::of(level), &text);
        for (level, line) in done {
            stage_log::log(level, &line);
        }
    });
}

/// What one log callback carries: the start of a message, at a stage-log
/// level, or more of the message before it.
#[derive(Clone, Copy, Debug, PartialEq)]
enum Piece {
    Start(&'static str),
    More,
}

impl Piece {
    fn of(level: ggml_log_level) -> Self {
        match level {
            GGML_LOG_LEVEL_CONT => Piece::More,
            GGML_LOG_LEVEL_ERROR => Piece::Start("warn"),
            GGML_LOG_LEVEL_DEBUG => Piece::Start("trace"),
            _ => Piece::Start("debug"),
        }
    }
}

/// Whole lines out of the pieces llama.cpp logs in. A message need not end
/// in a newline: the next piece continues it (progress dots), or the next
/// message ends it.
struct Lines {
    level: &'static str,
    partial: String,
}

impl Lines {
    const fn new() -> Self {
        Lines {
            level: "debug",
            partial: String::new(),
        }
    }

    /// Take one piece; return the lines it completes, each with its level.
    /// Blank lines are dropped.
    fn push(&mut self, piece: Piece, text: &str) -> Vec<(&'static str, String)> {
        let mut done = Vec::new();
        if let Piece::Start(level) = piece {
            let unfinished = std::mem::take(&mut self.partial);
            keep(&mut done, self.level, &unfinished);
            self.level = level;
        }
        self.partial.push_str(text);
        while let Some(end) = self.partial.find('\n') {
            let line: String = self.partial.drain(..=end).collect();
            keep(&mut done, self.level, &line);
        }
        done
    }
}

fn keep(done: &mut Vec<(&'static str, String)>, level: &'static str, line: &str) {
    let line = line.trim_end();
    if !line.trim_start().is_empty() {
        done.push((level, line.to_string()));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn lines(pieces: &[(Piece, &str)]) -> Vec<(&'static str, String)> {
        let mut l = Lines::new();
        pieces
            .iter()
            .flat_map(|(piece, text)| l.push(*piece, text))
            .collect()
    }

    #[test]
    fn errors_become_warnings_and_the_rest_goes_below_info() {
        assert_eq!(Piece::of(GGML_LOG_LEVEL_ERROR), Piece::Start("warn"));
        assert_eq!(Piece::of(GGML_LOG_LEVEL_DEBUG), Piece::Start("trace"));
        assert_eq!(Piece::of(GGML_LOG_LEVEL_CONT), Piece::More);
        for quiet in [
            llama_cpp_sys_2::GGML_LOG_LEVEL_NONE,
            llama_cpp_sys_2::GGML_LOG_LEVEL_INFO,
            llama_cpp_sys_2::GGML_LOG_LEVEL_WARN,
        ] {
            assert_eq!(Piece::of(quiet), Piece::Start("debug"));
        }
    }

    #[test]
    fn a_message_is_one_line_per_newline() {
        assert_eq!(
            lines(&[(Piece::Start("debug"), "first\nsecond\n")]),
            [
                ("debug", "first".to_string()),
                ("debug", "second".to_string())
            ]
        );
    }

    #[test]
    fn continued_pieces_join_the_line_they_continue() {
        assert_eq!(
            lines(&[
                (Piece::Start("warn"), "loading"),
                (Piece::More, "."),
                (Piece::More, ".\n"),
                (Piece::Start("debug"), "next\n"),
            ]),
            [
                ("warn", "loading..".to_string()),
                ("debug", "next".to_string())
            ]
        );
    }

    #[test]
    fn a_new_message_ends_one_left_unfinished() {
        assert_eq!(
            lines(&[
                (Piece::Start("debug"), "no newline"),
                (Piece::Start("warn"), "an error\n"),
            ]),
            [
                ("debug", "no newline".to_string()),
                ("warn", "an error".to_string())
            ]
        );
    }

    #[test]
    fn blank_lines_are_dropped() {
        assert!(lines(&[(Piece::Start("debug"), "\n  \n"), (Piece::More, "\n")]).is_empty());
    }
}
