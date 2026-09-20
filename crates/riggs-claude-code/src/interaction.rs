use crate::profile::Profile;
use crate::sign_in::Ask as SignIn;
use rax::id::PromptId;
use rax::interaction::{
    DisplayAnswer, DisplayOutcome, PlanChoice, PlanRequest, Question, QuestionOption,
    QuestionRequest,
};
use serde_json::{Map, Value, json};
use uuid::Uuid;

pub(crate) const PLAN_TITLE: &str = "Plan — approve?";

#[derive(Debug)]
pub(crate) struct ToolResult {
    pub(crate) text: String,
    pub(crate) is_error: bool,
}

impl ToolResult {
    pub(crate) fn ok(value: Value) -> Self {
        Self {
            text: value.to_string(),
            is_error: false,
        }
    }

    pub(crate) fn text(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            is_error: false,
        }
    }

    pub(crate) fn error(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            is_error: true,
        }
    }
}

#[derive(Debug)]
pub(crate) struct Ask {
    plain: bool,
    questions: Vec<Question>,
}

/// Reads the `auth` tool's arguments: a capability name plus either a built-in profile or a
/// command of the caller's own.
pub(crate) fn parse_auth(
    args: &Value,
    config: &crate::config::ClaudeCodeConfig,
) -> Result<SignIn, ToolResult> {
    let tool = blank_to_none(args, "tool").ok_or_else(|| {
        ToolResult::error(
            "Error: `tool` is required: name the capability that needs authentication",
        )
    })?;
    let profile = blank_to_none(args, "profile").ok_or_else(|| {
        ToolResult::error(format!(
            "Error: `profile` is required: one of {}",
            crate::profile::NAMES.join(", ")
        ))
    })?;
    let command = blank_to_none(args, "command");
    let needs_code = args
        .get("needs_code")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if profile == crate::profile::CUSTOM {
        let command = command.ok_or_else(|| {
            ToolResult::error("Error: the `custom` profile needs a `command` to run")
        })?;
        let mut words = command.split_whitespace().map(str::to_owned);
        let program = words.next().ok_or_else(|| {
            ToolResult::error("Error: the `custom` profile needs a `command` to run")
        })?;
        return Ok(SignIn {
            profile: Profile::custom(program.into(), words.collect(), needs_code),
            tool,
            approve_first: true,
        });
    }
    if command.is_some() {
        return Err(ToolResult::error(format!(
            "Error: `command` belongs to the `custom` profile only; {profile} runs its own"
        )));
    }
    let built = Profile::builtin(&profile, config).ok_or_else(|| {
        ToolResult::error(format!(
            "Error: there is no `{profile}` sign-in here; use one of {}",
            crate::profile::NAMES.join(", ")
        ))
    })?;
    Ok(SignIn {
        profile: built,
        tool,
        approve_first: false,
    })
}

pub(crate) fn prompt_id() -> PromptId {
    PromptId(Uuid::new_v4().to_string())
}

pub(crate) fn parse_ask(args: &Value) -> Result<Ask, ToolResult> {
    let items: Vec<&Value> = match args.get("questions").and_then(Value::as_array) {
        Some(items) => items.iter().collect(),
        None => vec![args],
    };
    let mut questions = Vec::new();
    for (index, item) in items.into_iter().enumerate() {
        let question = blank_to_none(item, "question")
            .or_else(|| blank_to_none(item, "label"))
            .ok_or_else(|| ToolResult::error("Error: a question is required"))?;
        let options = parse_options(item.get("options"));
        if options.len() < 2 {
            return Err(ToolResult::error("Error: provide at least two options"));
        }
        questions.push(Question {
            key: format!("q{index}"),
            header: blank_to_none(item, "header"),
            question,
            options,
            multi_select: item
                .get("multiSelect")
                .and_then(Value::as_bool)
                .unwrap_or(false),
        });
    }
    if questions.is_empty() {
        return Err(ToolResult::error("Error: a question is required"));
    }
    let plain = matches!(questions.as_slice(), [only] if !only.multi_select
        && only.header.is_none()
        && only.options.iter().all(|option| option.description.is_none()));
    Ok(Ask { plain, questions })
}

fn blank_to_none(value: &Value, key: &str) -> Option<String> {
    value
        .get(key)
        .and_then(Value::as_str)
        .filter(|text| !text.trim().is_empty())
        .map(str::to_owned)
}

fn parse_options(options: Option<&Value>) -> Vec<QuestionOption> {
    options
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|option| match option {
            Value::String(label) => Some(QuestionOption {
                label: label.clone(),
                description: None,
            }),
            Value::Object(_) => Some(QuestionOption {
                label: blank_to_none(option, "label")?,
                description: blank_to_none(option, "description"),
            }),
            _ => None,
        })
        .filter(|option| !option.label.trim().is_empty())
        .collect()
}

impl Ask {
    pub(crate) fn request(&self, id: PromptId) -> QuestionRequest {
        QuestionRequest {
            id,
            title: None,
            questions: self.questions.clone(),
        }
    }

    pub(crate) fn result(&self, answer: DisplayAnswer) -> ToolResult {
        match answer.outcome {
            DisplayOutcome::Answered if self.plain => {
                let choice = answer
                    .answers
                    .get("q0")
                    .and_then(|choices| choices.first())
                    .cloned()
                    .unwrap_or_default();
                ToolResult::ok(json!({"answered": true, "choice": choice}))
            }
            DisplayOutcome::Answered => {
                let answers: Vec<Value> = self
                    .questions
                    .iter()
                    .map(|question| {
                        let choices = answer.answers.get(&question.key).cloned();
                        json!({"question": question.question, "choices": choices.unwrap_or_default()})
                    })
                    .collect();
                let mut result = Map::new();
                result.insert("answered".to_owned(), Value::Bool(true));
                result.insert("answers".to_owned(), Value::Array(answers));
                if let Some(user_id) = answer.user_id {
                    result.insert("user_id".to_owned(), Value::String(user_id));
                }
                ToolResult::ok(Value::Object(result))
            }
            DisplayOutcome::TimedOut => not_answered(
                "The user did not respond in time. Do not assume an answer — ask again or stop and wait.",
            ),
            DisplayOutcome::Dismissed if self.plain => {
                not_answered("The question was dismissed before the user answered.")
            }
            DisplayOutcome::Dismissed => {
                not_answered("The questions were dismissed before the user answered.")
            }
            DisplayOutcome::Chat => {
                let topics: Vec<String> = self
                    .questions
                    .iter()
                    .map(|question| format!("- {}", question.question))
                    .collect();
                not_answered(&format!(
                    "The user would rather talk this through than pick from the options. They asked: \"Can we chat about this?\"\n\nDiscuss these with them before deciding:\n{}",
                    topics.join("\n")
                ))
            }
            DisplayOutcome::NoConversation => {
                ToolResult::error("Error: there is no conversation to ask in")
            }
            DisplayOutcome::Denied | DisplayOutcome::Unavailable | DisplayOutcome::Approved => {
                ToolResult::error(answer.note.unwrap_or_else(|| {
                    "Error: interactive questions are not available in this context".to_owned()
                }))
            }
        }
    }
}

fn not_answered(note: &str) -> ToolResult {
    ToolResult::ok(json!({"answered": false, "note": note}))
}

pub(crate) fn parse_plan(args: &Value) -> Result<(String, String), ToolResult> {
    let plan = blank_to_none(args, "plan")
        .ok_or_else(|| ToolResult::error("Error: a plan is required"))?;
    let title = blank_to_none(args, "title").unwrap_or_else(|| PLAN_TITLE.to_owned());
    Ok((title, plan))
}

pub(crate) fn plan_request(id: PromptId, title: String, plan: String) -> PlanRequest {
    PlanRequest {
        id,
        title: Some(title),
        plan,
    }
}

pub(crate) fn plan_result(answer: DisplayAnswer) -> ToolResult {
    let not_approved = |note: &str| ToolResult::ok(json!({"approved": false, "note": note}));
    match answer.outcome {
        DisplayOutcome::Answered => match answer.choice {
            Some(PlanChoice::Proceed) => ToolResult::ok(json!({
                "approved": true,
                "choice": "Proceed",
                "note": "Approved — proceed with the plan as presented.",
            })),
            Some(PlanChoice::Revise) => ToolResult::ok(json!({
                "approved": false,
                "choice": "Revise",
                "note": "The user wants changes before you proceed. Ask what to adjust; do not start yet.",
            })),
            Some(PlanChoice::Cancel) => ToolResult::ok(json!({
                "approved": false,
                "choice": "Cancel",
                "note": "The user cancelled. Do not proceed.",
            })),
            None => ToolResult::ok(json!({"approved": false, "choice": ""})),
        },
        DisplayOutcome::TimedOut => {
            not_approved("No response in time. Do not assume approval — ask again or stop.")
        }
        DisplayOutcome::Dismissed => {
            not_approved("The plan prompt was dismissed before they answered.")
        }
        DisplayOutcome::Chat => not_approved(
            "The user wants to talk the plan over before deciding. Ask what they want to discuss; do not start yet.",
        ),
        DisplayOutcome::NoConversation => {
            ToolResult::error("Error: there is no conversation to present a plan in")
        }
        DisplayOutcome::Denied | DisplayOutcome::Unavailable | DisplayOutcome::Approved => {
            ToolResult::error(answer.note.unwrap_or_else(|| {
                "Error: interactive plan approval is not available in this context".to_owned()
            }))
        }
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    fn auth(args: Value) -> Result<SignIn, String> {
        super::parse_auth(&args, &crate::config::ClaudeCodeConfig::new("/tmp"))
            .map_err(|refused| refused.text)
    }

    #[test]
    fn a_built_in_sign_in_is_taken_by_name_and_runs_without_approval() {
        let ask = auth(json!({"tool": "gcp-mcp", "profile": "gcloud-adc"})).unwrap();
        assert_eq!(ask.tool, "gcp-mcp");
        assert_eq!(ask.profile.name, "gcloud-adc");
        assert!(!ask.approve_first);
    }

    #[test]
    fn a_custom_command_is_split_and_kept_for_the_owner_to_approve() {
        let ask = auth(json!({
            "tool": "widget-mcp",
            "profile": "custom",
            "command": " /bin/login --device  now ",
            "needs_code": true,
        }))
        .unwrap();
        assert_eq!(ask.profile.command_line(), "/bin/login --device now");
        assert!(ask.approve_first && ask.profile.needs_code);
    }

    #[test]
    fn an_auth_request_that_cannot_be_run_is_refused_with_what_to_fix() {
        for (args, wanted) in [
            (json!({"profile": "gcloud"}), "`tool` is required"),
            (json!({"tool": "x"}), "`profile` is required"),
            (
                json!({"tool": "x", "profile": "aws"}),
                "there is no `aws` sign-in",
            ),
            (
                json!({"tool": "x", "profile": "custom"}),
                "needs a `command`",
            ),
            (
                json!({"tool": "x", "profile": "gcloud", "command": "/bin/login"}),
                "`command` belongs to the `custom` profile only",
            ),
        ] {
            let refused = auth(args.clone()).unwrap_err();
            assert!(refused.contains(wanted), "{args}: {refused}");
        }
    }

    use super::*;

    fn answered(answers: &[(&str, &str)]) -> DisplayAnswer {
        DisplayAnswer {
            id: prompt_id(),
            outcome: DisplayOutcome::Answered,
            answers: answers
                .iter()
                .map(|(key, choice)| ((*key).to_owned(), vec![(*choice).to_owned()]))
                .collect(),
            choice: None,
            user_id: None,
            note: None,
            code: None,
        }
    }

    fn refused(args: Value) -> String {
        match parse_ask(&args) {
            Ok(_) => panic!("expected {args} to be refused"),
            Err(result) => result.text,
        }
    }

    #[test]
    fn a_plain_question_answers_with_a_single_choice() {
        let ask = parse_ask(&json!({"questions": [{"question": "Ship it?", "options": ["Yes", {"label": "No"}, {"label": " "}]}]})).unwrap();
        let request = ask.request(prompt_id());
        assert_eq!(request.questions[0].key, "q0");
        assert_eq!(request.questions[0].options.len(), 2);
        let result = ask.result(answered(&[("q0", "Yes")]));
        assert_eq!(result.text, r#"{"answered":true,"choice":"Yes"}"#);
        let dismissed = ask.result(DisplayAnswer {
            outcome: DisplayOutcome::Dismissed,
            ..answered(&[])
        });
        assert!(dismissed.text.contains("The question was dismissed"));
    }

    #[test]
    fn malformed_questions_are_refused_with_the_contract_wording() {
        assert_eq!(
            refused(json!({"questions": []})),
            "Error: a question is required"
        );
        assert_eq!(
            refused(json!({"question": "Pick", "options": ["only one"]})),
            "Error: provide at least two options"
        );
    }

    #[test]
    fn chat_and_no_conversation_read_as_answers_for_the_model() {
        let ask = parse_ask(&json!({"question": "Pick", "options": ["A", "B"]})).unwrap();
        let chat = ask.result(DisplayAnswer {
            outcome: DisplayOutcome::Chat,
            ..answered(&[])
        });
        assert!(!chat.is_error);
        assert!(
            chat.text
                .contains("Discuss these with them before deciding:\\n- Pick")
        );
        let nobody = ask.result(DisplayAnswer {
            outcome: DisplayOutcome::NoConversation,
            ..answered(&[])
        });
        assert!(nobody.is_error);
        assert_eq!(nobody.text, "Error: there is no conversation to ask in");
    }

    #[test]
    fn a_plan_defaults_its_title_and_maps_each_choice() {
        let (title, plan) = parse_plan(&json!({"plan": "do it"})).unwrap();
        assert_eq!((title.as_str(), plan.as_str()), (PLAN_TITLE, "do it"));
        assert!(parse_plan(&json!({"plan": "  "})).is_err());
        let revise = plan_result(DisplayAnswer {
            choice: Some(PlanChoice::Revise),
            ..answered(&[])
        });
        assert!(revise.text.contains(r#""approved":false"#) && revise.text.contains("Revise"));
    }
}
