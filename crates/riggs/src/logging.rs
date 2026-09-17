use std::io::{self, Write};

use tracing_subscriber::fmt::MakeWriter;

use crate::config::{LogConfig, LogFormat};
use crate::token::PREFIX;

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

pub fn redact(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(at) = rest.find(PREFIX) {
        let (before, from) = rest.split_at(at);
        out.push_str(before);
        let body = &from[PREFIX.len()..];
        let end = body
            .find(|c: char| !(c.is_ascii_alphanumeric() || c == '_' || c == '-'))
            .unwrap_or(body.len());
        out.push_str(PREFIX);
        if end > 0 {
            out.push_str(REDACTED);
        }
        rest = &body[end..];
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
    }
}
