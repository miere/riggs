use std::io::{self, Write};

use tracing_subscriber::fmt::MakeWriter;

use crate::config::{LogConfig, LogFormat};
use crate::token::{MARKER, is_gateway_name};

const REDACTED: &str = "[redacted]";

pub fn init(config: &LogConfig) {
    let builder = tracing_subscriber::fmt()
        .with_max_level(config.level)
        .with_ansi(false)
        .with_writer(Redacting);
    let _ = match config.format {
        LogFormat::Text => builder.try_init(),
        LogFormat::Json => builder.json().try_init(),
    };
}

struct Redacting;

impl<'a> MakeWriter<'a> for Redacting {
    type Writer = RedactingWriter;

    fn make_writer(&'a self) -> Self::Writer {
        RedactingWriter
    }
}

struct RedactingWriter;

impl Write for RedactingWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let text = String::from_utf8_lossy(buf);
        io::stderr().lock().write_all(redact(&text).as_bytes())?;
        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        io::stderr().flush()
    }
}

/// Hides the body of anything shaped like a node token, `<gateway>_node_…`, whichever gateway
/// minted it.
pub fn redact(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(at) = rest.find(MARKER) {
        let (before, from) = rest.split_at(at);
        let name_start = before
            .rfind(|c: char| !(c.is_ascii_alphanumeric() || c == '_'))
            .map_or(0, |i| i + 1);
        let body = &from[MARKER.len()..];
        let end = body
            .find(|c: char| !(c.is_ascii_alphanumeric() || c == '_' || c == '-'))
            .unwrap_or(body.len());
        out.push_str(before);
        out.push_str(MARKER);
        if end > 0 && is_gateway_name(&before[name_start..]) {
            out.push_str(REDACTED);
            rest = &body[end..];
        } else {
            rest = body;
        }
    }
    out.push_str(rest);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn anything_shaped_like_a_node_token_is_redacted() {
        assert_eq!(
            redact("bad token mrtg_node_0123456789abcdef_se_cr-et\" here, mrtg_node_x"),
            "bad token mrtg_node_[redacted]\" here, mrtg_node_[redacted]"
        );
        assert_eq!(redact("no token"), "no token");
        assert_eq!(redact("mrtg_node_ alone"), "mrtg_node_ alone");
        assert_eq!(
            redact("kepler_node_0123456789abcdef_s3cr-et and rax_node_x"),
            "kepler_node_[redacted] and rax_node_[redacted]"
        );
        assert_eq!(redact("Bad_node_x _node_x"), "Bad_node_x _node_x");
    }
}
