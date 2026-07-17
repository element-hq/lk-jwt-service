// Copyright 2026 Element Creations Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use tokio::io::{AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpListener;
use tokio::net::tcp::OwnedReadHalf;

pub struct FakeRedis {
    url: String,
    state: Arc<Mutex<HashMap<String, HashMap<String, String>>>>,
}

impl FakeRedis {
    /// Start a new fake Redis server. The listener task lives on the test's
    /// tokio runtime and dies with it.
    pub async fn new() -> FakeRedis {
        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .expect("failed to bind fake Redis listener");
        let url = format!("redis://{}/0", listener.local_addr().unwrap());
        let state = Arc::new(Mutex::new(HashMap::new()));

        let accept_state = Arc::clone(&state);
        tokio::spawn(async move {
            loop {
                let Ok((stream, _)) = listener.accept().await else {
                    return;
                };
                tokio::spawn(handle_connection(stream, Arc::clone(&accept_state)));
            }
        });

        FakeRedis { url, state }
    }

    pub fn url(&self) -> &str {
        &self.url
    }

    pub fn hash_field_exists(&self, hash_key: &str, field: &str) -> bool {
        self.state
            .lock()
            .unwrap()
            .get(hash_key)
            .is_some_and(|hash| hash.contains_key(field))
    }
}

async fn handle_connection(
    stream: tokio::net::TcpStream,
    state: Arc<Mutex<HashMap<String, HashMap<String, String>>>>,
) {
    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);
    loop {
        let Some(args) = read_command(&mut reader).await else {
            return;
        };
        if args.is_empty() {
            continue;
        }
        let reply = dispatch(&args, &state);
        if writer.write_all(&reply).await.is_err() {
            return;
        }
    }
}

/// Read one RESP2 request.
async fn read_command(reader: &mut BufReader<OwnedReadHalf>) -> Option<Vec<String>> {
    let header = read_line(reader).await?;
    let argc: usize = header.strip_prefix('*')?.parse().ok()?;

    let mut args = Vec::with_capacity(argc);
    for _ in 0..argc {
        let len_line = read_line(reader).await?;
        let len: usize = len_line.strip_prefix('$')?.parse().ok()?;

        let mut buf = vec![0u8; len + 2]; // Payload + trailing CRLF
        reader.read_exact(&mut buf).await.ok()?;
        buf.truncate(len);
        args.push(String::from_utf8(buf).ok()?);
    }
    Some(args)
}

/// Read a single line without the trailing CRLF.
async fn read_line(reader: &mut BufReader<OwnedReadHalf>) -> Option<String> {
    let mut line = Vec::new();
    loop {
        let byte = reader.read_u8().await.ok()?;
        if byte == b'\n' {
            if line.last() == Some(&b'\r') {
                line.pop();
            }
            return String::from_utf8(line).ok();
        }
        line.push(byte);
    }
}

/// Handle a parsed command and return its raw RESP2 reply.
fn dispatch(
    args: &[String],
    state: &Arc<Mutex<HashMap<String, HashMap<String, String>>>>,
) -> Vec<u8> {
    match args[0].to_ascii_uppercase().as_str() {
        "PING" => b"+PONG\r\n".to_vec(),

        // Deliberately unsupported. Callers should use this to decide whether to fall
        // back to RESP2.
        "HELLO" => b"-ERR unknown command 'HELLO'\r\n".to_vec(),

        "HSET" => {
            let mut state = state.lock().unwrap();
            let hash = state.entry(args[1].clone()).or_default();
            let mut added = 0;
            for pair in args[2..].chunks_exact(2) {
                if hash.insert(pair[0].clone(), pair[1].clone()).is_none() {
                    added += 1;
                }
            }
            format!(":{added}\r\n").into_bytes()
        }

        "HDEL" => {
            let mut state = state.lock().unwrap();
            let removed = state
                .get_mut(&args[1])
                .map(|hash| {
                    args[2..]
                        .iter()
                        .filter(|f| hash.remove(*f).is_some())
                        .count()
                })
                .unwrap_or(0);
            format!(":{removed}\r\n").into_bytes()
        }

        "HGETALL" => {
            let state = state.lock().unwrap();
            let empty = HashMap::new();
            let hash = state.get(&args[1]).unwrap_or(&empty);
            let mut reply = format!("*{}\r\n", hash.len() * 2).into_bytes();
            for (field, value) in hash {
                reply.extend(format!("${}\r\n{field}\r\n", field.len()).into_bytes());
                reply.extend(format!("${}\r\n{value}\r\n", value.len()).into_bytes());
            }
            reply
        }

        _ => b"-ERR unknown command\r\n".to_vec(),
    }
}
