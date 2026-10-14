use rax::content::ContentBlock;
use rax::open::{Subject, UnhandledReason};
use rax::{Open, Unhandled};
use serde_json::{Value, json};

pub(crate) struct Converted {
    pub(crate) blocks: Vec<Value>,
    pub(crate) unhandled: Vec<Unhandled>,
}

pub(crate) fn prompt(content: Vec<Open<ContentBlock>>) -> Converted {
    let mut converted = Converted {
        blocks: Vec::new(),
        unhandled: Vec::new(),
    };
    for (index, block) in content.into_iter().enumerate() {
        match block {
            Open::Known(ContentBlock::Text { text }) => {
                if !text.is_empty() {
                    converted.blocks.push(text_block(&text));
                }
            }
            Open::Known(ContentBlock::Image {
                data, mime_type, ..
            }) => converted.blocks.push(json!({
                "type": "image",
                "source": {"type": "base64", "media_type": mime_type, "data": data},
            })),
            Open::Known(ContentBlock::ResourceLink { uri, name, .. }) => {
                converted.blocks.push(text_block(&link(&name, &uri)));
            }
            Open::Known(ContentBlock::Audio { .. } | ContentBlock::Resource { .. })
            | Open::Unknown { .. } => converted.unhandled.push(Unhandled {
                subject: Subject::Block { index },
                reason: UnhandledReason::UnsupportedType,
                message: Some("Claude Code cannot read this kind of content".to_owned()),
            }),
        }
    }
    converted
}

pub(crate) fn context(context: &[Open<ContentBlock>]) -> (Option<String>, Vec<Unhandled>) {
    let mut lines = Vec::new();
    let mut unhandled = Vec::new();
    for (index, block) in context.iter().enumerate() {
        match block {
            Open::Known(ContentBlock::Text { text }) => lines.push(text.clone()),
            Open::Known(ContentBlock::ResourceLink { uri, name, .. }) => {
                lines.push(link(name, uri));
            }
            _ => unhandled.push(Unhandled {
                subject: Subject::Block { index },
                reason: UnhandledReason::UnsupportedType,
                message: Some("Claude Code cannot read this kind of context".to_owned()),
            }),
        }
    }
    let text = lines.join("\n");
    ((!text.trim().is_empty()).then_some(text), unhandled)
}

/// A gateway tool's answer, with what the node fetched for it on lines of their own: a local file
/// as its path, and a file it could not fetch as the note saying why.
pub(crate) fn tool_outcome(content: String, resources: &[Open<ContentBlock>]) -> String {
    let mut text = content;
    for resource in resources {
        let line = match resource {
            Open::Known(ContentBlock::Text { text }) => text.clone(),
            Open::Known(ContentBlock::ResourceLink { uri, name, .. }) => link(name, uri),
            _ => continue,
        };
        if !text.is_empty() {
            text.push('\n');
        }
        text.push_str(&line);
    }
    text
}

pub(crate) fn text_block(text: &str) -> Value {
    json!({"type": "text", "text": text})
}

fn link(name: &str, uri: &str) -> String {
    format!("[{name}: {uri}]")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_tool_outcome_lists_its_files_after_the_answer() {
        let resources = [
            ContentBlock::link("file:///tmp/s1/x-q3.pdf", "Q3 report.pdf").into(),
            ContentBlock::text("[secret.pdf: the file could not be fetched: not yours]").into(),
        ];
        assert_eq!(
            tool_outcome("Here it is.".to_owned(), &resources),
            "Here it is.\n[Q3 report.pdf: file:///tmp/s1/x-q3.pdf]\n\
             [secret.pdf: the file could not be fetched: not yours]"
        );
        assert_eq!(
            tool_outcome("Nothing lent.".to_owned(), &[]),
            "Nothing lent."
        );
    }
}
