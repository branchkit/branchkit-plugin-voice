//! The request body and the prefix rule, free of llama.cpp so they are tested
//! without a model.

use serde::Deserialize;
use serde_json::Value;

/// The most tokens a request may ask to be generated.
pub const MAX_TOKENS_CAP: u32 = 1024;
const MAX_TOKENS_DEFAULT: u32 = 256;

#[derive(Debug, Deserialize, PartialEq)]
pub struct Message {
    pub role: String,
    pub content: String,
}

#[derive(Debug, PartialEq)]
pub struct ChatRequest {
    pub messages: Vec<Message>,
    pub max_tokens: u32,
}

/// Read a request body: a non-empty `messages` list of `{role, content}`,
/// with an optional `max_tokens` (default 256, at most 1024).
pub fn parse_request(body: &Value) -> Result<ChatRequest, String> {
    let messages: Vec<Message> = serde_json::from_value(
        body.get("messages")
            .cloned()
            .ok_or("expected {\"messages\": [{\"role\", \"content\"}, ...]}")?,
    )
    .map_err(|e| format!("bad messages: {e}"))?;
    if messages.is_empty() {
        return Err("messages is empty".into());
    }
    let max_tokens = match body.get("max_tokens") {
        None => MAX_TOKENS_DEFAULT,
        Some(v) => v
            .as_u64()
            .filter(|n| *n > 0)
            .ok_or("max_tokens must be a positive integer")?
            .min(MAX_TOKENS_CAP as u64) as u32,
    };
    Ok(ChatRequest {
        messages,
        max_tokens,
    })
}

/// How many leading items two sequences share: the part of the cache the
/// next prompt can keep.
pub fn common_prefix<T: PartialEq>(a: &[T], b: &[T]) -> usize {
    a.iter().zip(b).take_while(|(x, y)| x == y).count()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn a_request_names_messages_and_may_bound_its_reply() {
        let r = parse_request(&json!({
            "messages": [{"role": "system", "content": "s"}, {"role": "user", "content": "u"}],
            "max_tokens": 5000,
        }))
        .unwrap();
        assert_eq!(r.messages.len(), 2);
        assert_eq!(r.max_tokens, MAX_TOKENS_CAP);
        let r = parse_request(&json!({"messages": [{"role": "user", "content": "u"}]})).unwrap();
        assert_eq!(r.max_tokens, 256);
    }

    #[test]
    fn a_malformed_request_is_refused_with_the_reason() {
        assert!(parse_request(&json!({})).is_err());
        assert!(parse_request(&json!({"messages": []})).is_err());
        assert!(parse_request(&json!({"messages": [{"role": "user"}]})).is_err());
        assert!(
            parse_request(&json!({"messages": [{"role": "u", "content": "c"}], "max_tokens": 0}))
                .is_err()
        );
    }

    #[test]
    fn the_cache_keeps_the_shared_prefix() {
        assert_eq!(common_prefix(&[1, 2, 3, 4], &[1, 2, 9]), 2);
        assert_eq!(common_prefix::<i32>(&[], &[1]), 0);
        assert_eq!(common_prefix(&[1, 2], &[1, 2, 3]), 2);
    }
}
