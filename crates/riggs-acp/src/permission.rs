use rax::Decision;
use rax::tool::DeniedBy;
use serde_json::{Value, json};

use crate::wire::{OptionKind, PermissionOption};

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Answer {
    Selected { option_id: String, allowed: bool },
    Cancelled,
}

impl Answer {
    pub(crate) fn allowed(&self) -> bool {
        matches!(self, Self::Selected { allowed: true, .. })
    }
}

/// `allow_always` is never picked: the agent would stop asking, and a later deny could no
/// longer stop the tool.
pub(crate) fn choose(options: &[PermissionOption], decision: &Decision) -> Answer {
    let first = |kind: OptionKind| options.iter().find(|option| option.kind == kind);
    let allow = match decision {
        Decision::Allow => first(OptionKind::AllowOnce),
        Decision::Deny { .. } => None,
    };
    if let Some(option) = allow {
        return Answer::Selected {
            option_id: option.id.clone(),
            allowed: true,
        };
    }
    match first(OptionKind::RejectOnce).or_else(|| first(OptionKind::RejectAlways)) {
        Some(option) => Answer::Selected {
            option_id: option.id.clone(),
            allowed: false,
        },
        None => Answer::Cancelled,
    }
}

/// ACP has no field for a deny reason, so it rides in `_meta` for agents that look.
pub(crate) fn response(answer: &Answer, decision: Option<&Decision>) -> Value {
    let outcome = match answer {
        Answer::Selected { option_id, .. } => json!({"outcome": "selected", "optionId": option_id}),
        Answer::Cancelled => json!({"outcome": "cancelled"}),
    };
    match decision {
        Some(Decision::Deny { by, reason }) => json!({
            "outcome": outcome,
            "_meta": {"riggs": {"by": denied_by(*by), "reason": reason}},
        }),
        _ => json!({"outcome": outcome}),
    }
}

fn denied_by(by: DeniedBy) -> Value {
    serde_json::to_value(by).unwrap_or(Value::Null)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn option(id: &str, kind: OptionKind) -> PermissionOption {
        PermissionOption {
            id: id.into(),
            kind,
        }
    }

    fn deny() -> Decision {
        Decision::Deny {
            by: DeniedBy::Policy,
            reason: Some("no".into()),
        }
    }

    fn selected(id: &str, allowed: bool) -> Answer {
        Answer::Selected {
            option_id: id.into(),
            allowed,
        }
    }

    #[test]
    fn allow_picks_allow_once_even_when_always_comes_first() {
        let options = [
            option("always", OptionKind::AllowAlways),
            option("once", OptionKind::AllowOnce),
            option("no", OptionKind::RejectOnce),
        ];
        assert_eq!(choose(&options, &Decision::Allow), selected("once", true));
    }

    #[test]
    fn allow_without_allow_once_rejects_rather_than_remembering() {
        let options = [
            option("always", OptionKind::AllowAlways),
            option("never", OptionKind::RejectAlways),
        ];
        assert_eq!(choose(&options, &Decision::Allow), selected("never", false));
        let only_always = [option("always", OptionKind::AllowAlways)];
        assert_eq!(choose(&only_always, &Decision::Allow), Answer::Cancelled);
    }

    #[test]
    fn deny_prefers_reject_once_then_reject_always_then_cancelled() {
        let both = [
            option("never", OptionKind::RejectAlways),
            option("no", OptionKind::RejectOnce),
        ];
        assert_eq!(choose(&both, &deny()), selected("no", false));
        let always = [
            option("once", OptionKind::AllowOnce),
            option("never", OptionKind::RejectAlways),
        ];
        assert_eq!(choose(&always, &deny()), selected("never", false));
        let none = [option("once", OptionKind::AllowOnce)];
        assert_eq!(choose(&none, &deny()), Answer::Cancelled);
        assert_eq!(choose(&[], &Decision::Allow), Answer::Cancelled);
    }

    #[test]
    fn responses_use_the_acp_outcome_shape_and_carry_the_reason() {
        assert_eq!(
            response(&selected("once", true), Some(&Decision::Allow)),
            json!({"outcome": {"outcome": "selected", "optionId": "once"}})
        );
        assert_eq!(
            response(&Answer::Cancelled, None),
            json!({"outcome": {"outcome": "cancelled"}})
        );
        assert_eq!(
            response(&selected("no", false), Some(&deny())),
            json!({
                "outcome": {"outcome": "selected", "optionId": "no"},
                "_meta": {"riggs": {"by": "policy", "reason": "no"}},
            })
        );
    }
}
