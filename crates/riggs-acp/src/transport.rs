use std::io;

use agent_client_protocol::Lines;
use futures::{Sink, Stream};
use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};

/// Large enough for a real image in a message chunk, small enough to bound memory.
pub(crate) const MAX_LINE_BYTES: usize = 64 * 1024 * 1024;

pub(crate) type AgentLines = Lines<
    std::pin::Pin<Box<dyn Sink<String, Error = io::Error> + Send>>,
    std::pin::Pin<Box<dyn Stream<Item = io::Result<String>> + Send>>,
>;

pub(crate) fn lines<W, R>(stdin: W, stdout: R, max: usize) -> AgentLines
where
    W: AsyncWrite + Send + Unpin + 'static,
    R: AsyncRead + Send + Unpin + 'static,
{
    let outgoing = futures::sink::unfold(stdin, async |mut writer: W, line: String| {
        writer.write_all(line.as_bytes()).await?;
        writer.write_all(b"\n").await?;
        writer.flush().await?;
        Ok::<_, io::Error>(writer)
    });
    let reader = Some(LineReader {
        reader: BufReader::new(stdout),
        max,
    });
    let incoming = futures::stream::unfold(reader, async |reader| {
        let mut reader = reader?;
        match reader.next_line().await {
            Ok(Some(line)) => Some((Ok(line), Some(reader))),
            Ok(None) => None,
            Err(err) => Some((Err(err), None)),
        }
    });
    Lines::new(Box::pin(outgoing), Box::pin(incoming))
}

struct LineReader<R> {
    reader: BufReader<R>,
    max: usize,
}

impl<R: AsyncRead + Unpin> LineReader<R> {
    async fn next_line(&mut self) -> io::Result<Option<String>> {
        loop {
            let Some(line) = self.raw_line().await? else {
                return Ok(None);
            };
            if line.iter().all(u8::is_ascii_whitespace) {
                continue;
            }
            let text = String::from_utf8(line).map_err(|_| {
                io::Error::new(
                    io::ErrorKind::InvalidData,
                    "the agent wrote a line that is not UTF-8",
                )
            })?;
            if serde_json::from_str::<serde::de::IgnoredAny>(&text).is_err() {
                let shown: String = text.chars().take(200).collect();
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("the agent wrote a line that is not JSON: {shown}"),
                ));
            }
            return Ok(Some(text));
        }
    }

    async fn raw_line(&mut self) -> io::Result<Option<Vec<u8>>> {
        let mut line = Vec::new();
        loop {
            let buffer = self.reader.fill_buf().await?;
            if buffer.is_empty() {
                return Ok((!line.is_empty()).then_some(line));
            }
            let (take, done) = match buffer.iter().position(|byte| *byte == b'\n') {
                Some(newline) => (newline, true),
                None => (buffer.len(), false),
            };
            if line.len() + take > self.max {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("the agent wrote a line longer than {} bytes", self.max),
                ));
            }
            line.extend_from_slice(&buffer[..take]);
            self.reader.consume(if done { take + 1 } else { take });
            if done {
                if line.last() == Some(&b'\r') {
                    line.pop();
                }
                return Ok(Some(line));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use super::*;

    async fn read_all(input: &[u8], max: usize) -> Vec<io::Result<Option<String>>> {
        let mut reader = LineReader {
            reader: BufReader::with_capacity(4, input),
            max,
        };
        let mut out = Vec::new();
        loop {
            let next = reader.next_line().await;
            let stop = !matches!(next, Ok(Some(_)));
            out.push(next);
            if stop {
                return out;
            }
        }
    }

    #[tokio::test]
    async fn lines_split_across_reads_and_blank_lines_are_skipped() {
        let lines = read_all(b"{\"a\":1}\r\n\n  \n[1,2]", 64).await;
        let texts: Vec<_> = lines.into_iter().map(|line| line.unwrap()).collect();
        assert_eq!(
            texts,
            [Some("{\"a\":1}".to_owned()), Some("[1,2]".to_owned()), None]
        );
    }

    #[tokio::test]
    async fn an_oversize_line_fails_instead_of_growing() {
        let lines = read_all(b"{\"a\":\"0123456789\"}\n", 10).await;
        let Err(err) = &lines[0] else {
            panic!("expected a failure, got {lines:?}");
        };
        assert!(err.to_string().contains("longer than 10 bytes"));
    }

    #[tokio::test]
    async fn a_line_that_is_not_json_fails() {
        let lines = read_all(b"{\"ok\":true}\nhello\n", 64).await;
        assert!(matches!(&lines[0], Ok(Some(_))));
        let Err(err) = &lines[1] else {
            panic!("expected a failure, got {lines:?}");
        };
        assert!(err.to_string().contains("not JSON: hello"));
    }
}
