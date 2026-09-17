use rax::event::StopReason;

use crate::error::ClaudeCodeError;
use crate::wire::{RESUME_MISS, ResultFrame};

pub(crate) enum Outcome {
    Stray,
    ResumeMiss,
    Aborted {
        subtype: String,
        errors: Vec<String>,
    },
    Api {
        status: Option<u16>,
        message: String,
    },
    Completed(StopReason),
}

pub(crate) fn of(result: ResultFrame) -> Outcome {
    let ResultFrame {
        subtype,
        is_error,
        stop_reason,
        terminal_reason,
        num_turns,
        api_error_status,
        errors,
        result,
    } = result;
    let errors = errors.unwrap_or_default();
    let stop_reason = stop_reason.filter(|reason| !reason.is_empty());
    if subtype == "error_during_execution"
        && num_turns.unwrap_or(0) == 0
        && errors.iter().any(|error| error.starts_with(RESUME_MISS))
    {
        return Outcome::ResumeMiss;
    }
    if is_error && (terminal_reason.as_deref() == Some("api_error") || api_error_status.is_some()) {
        let message = result
            .filter(|text| !text.trim().is_empty())
            .unwrap_or_else(|| errors.join("; "));
        return Outcome::Api {
            status: api_error_status,
            message,
        };
    }
    let known = matches!(subtype.as_str(), "" | "success" | "error_max_turns");
    if subtype == "error_during_execution" || (!known && is_error) {
        return Outcome::Aborted { subtype, errors };
    }
    if subtype == "success" && num_turns == Some(0) && stop_reason.is_none() {
        return Outcome::Stray;
    }
    if subtype == "error_max_turns" {
        return Outcome::Completed(StopReason::MaxTurnRequests);
    }
    Outcome::Completed(match stop_reason.as_deref() {
        None | Some("end_turn" | "stop_sequence") => StopReason::EndTurn,
        Some("max_tokens") => StopReason::MaxTokens,
        Some("refusal") => StopReason::Refusal,
        Some(_) => StopReason::Other,
    })
}

impl Outcome {
    pub(crate) fn failure(self, api_error: Option<String>) -> Result<StopReason, ClaudeCodeError> {
        match self {
            Self::Completed(stop) => Ok(stop),
            Self::Stray => Ok(StopReason::EndTurn),
            Self::ResumeMiss => Err(ClaudeCodeError::Aborted {
                subtype: "error_during_execution".to_owned(),
                errors: vec![RESUME_MISS.to_owned()],
            }),
            Self::Aborted { subtype, errors } => Err(ClaudeCodeError::Aborted { subtype, errors }),
            Self::Api { status, message } => Err(ClaudeCodeError::Api {
                status,
                error: api_error,
                message,
            }),
        }
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use serde_json::json;

    use super::*;

    fn outcome(frame: serde_json::Value) -> Outcome {
        of(serde_json::from_value(frame).unwrap())
    }

    #[test]
    fn results_are_classified_by_subtype_not_stop_reason() {
        let cases = [
            (
                json!({"subtype": "success", "num_turns": 1, "stop_reason": "end_turn"}),
                "completed end_turn",
            ),
            (
                json!({"subtype": "success", "num_turns": 0, "stop_reason": null, "result": ""}),
                "stray",
            ),
            (
                json!({"subtype": "success", "num_turns": 1, "stop_reason": null, "result": ""}),
                "completed end_turn",
            ),
            (
                json!({"subtype": "error_during_execution", "is_error": true, "stop_reason": "end_turn", "num_turns": 3}),
                "aborted",
            ),
            (
                json!({"subtype": "error_max_turns", "is_error": true, "stop_reason": "end_turn"}),
                "completed max_turn_requests",
            ),
            (
                json!({"subtype": "error_something_new", "is_error": true}),
                "aborted",
            ),
            (
                json!({"subtype": "error_something_new", "is_error": false}),
                "completed end_turn",
            ),
            (
                json!({"subtype": "error_during_execution", "is_error": true, "num_turns": 0, "stop_reason": null, "errors": ["No conversation found with session ID: 4747ab33-07cd-40b6-acf1-e951a24ac869"]}),
                "resume miss",
            ),
            (
                json!({"subtype": "success", "is_error": true, "num_turns": 1, "stop_reason": "stop_sequence", "terminal_reason": "api_error", "api_error_status": 529, "result": "API Error: 529 Overloaded."}),
                "api 529",
            ),
        ];
        for (frame, expected) in cases {
            let described = match outcome(frame.clone()) {
                Outcome::Stray => "stray".to_owned(),
                Outcome::ResumeMiss => "resume miss".to_owned(),
                Outcome::Aborted { .. } => "aborted".to_owned(),
                Outcome::Api { status, .. } => format!("api {}", status.unwrap()),
                Outcome::Completed(StopReason::EndTurn) => "completed end_turn".to_owned(),
                Outcome::Completed(StopReason::MaxTurnRequests) => {
                    "completed max_turn_requests".to_owned()
                }
                Outcome::Completed(other) => format!("completed {other:?}"),
            };
            assert_eq!(described, expected, "{frame}");
        }
    }
}
