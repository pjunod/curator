use anyhow::{Context, Result, bail};
use candle_core::{Device, Tensor};
use candle_nn::VarBuilder;
use candle_transformers::models::bert::{BertModel, Config, DTYPE};
use rayon::ThreadPool;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::io::{self, BufRead, Read, Write};
use std::path::{Path, PathBuf};
use tokenizers::{Tokenizer, TruncationParams};

const MODEL_ID: &str = "all-MiniLM-L6-v2@1110a243fdf4706b3f48f1d95db1a4f5529b4d41:tokenizers-0.22-onig-truncate256:unmasked-token-mean-l2:curator-metadata-v1";
const DIM: usize = 384;
const MAX_BATCH: usize = 8;
const MAX_TEXT_BYTES: usize = 8192;
// A decoded byte may become a six-byte JSON escape (for example, NUL).
// Reserve room for the fixed protocol keys, the pinned model ID and request ID.
const MAX_LINE_BYTES: usize = MAX_BATCH * MAX_TEXT_BYTES * 6 + 1024;
const FILES: [(&str, usize, &str); 3] = [
    (
        "config.json",
        612,
        "953f9c0d463486b10a6871cc2fd59f223b2c70184f49815e7efbcab5d8908b41",
    ),
    (
        "tokenizer.json",
        466247,
        "be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037",
    ),
    (
        "model.safetensors",
        90868376,
        "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db",
    ),
];

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Request {
    protocol_version: u32,
    request_id: String,
    model_id: String,
    texts: Vec<String>,
}

#[derive(Serialize)]
struct Response<'a> {
    protocol_version: u32,
    request_id: &'a str,
    model_id: &'static str,
    vectors: Option<Vec<Vec<f32>>>,
    error: Option<&'static str>,
}

struct Encoder {
    model: BertModel,
    tokenizer: Tokenizer,
    pool: ThreadPool,
}

fn read_verified(source: impl Read, size: usize, sha256: &str) -> Result<Vec<u8>> {
    let mut bytes = Vec::new();
    source.take((size + 1) as u64).read_to_end(&mut bytes)?;
    if bytes.len() != size || format!("{:x}", Sha256::digest(&bytes)) != sha256 {
        bail!("model file failed manifest verification");
    }
    Ok(bytes)
}

fn verified_file(dir: &Path, name: &str, size: usize, sha256: &str) -> Result<Vec<u8>> {
    let file = std::fs::File::open(dir.join(name)).with_context(|| format!("open {name}"))?;
    read_verified(file, size, sha256).with_context(|| format!("verify {name}"))
}

impl Encoder {
    fn load(dir: &Path) -> Result<Self> {
        let config_bytes = verified_file(dir, FILES[0].0, FILES[0].1, FILES[0].2)?;
        let tokenizer_bytes = verified_file(dir, FILES[1].0, FILES[1].1, FILES[1].2)?;
        let weights = verified_file(dir, FILES[2].0, FILES[2].1, FILES[2].2)?;
        let config: Config = serde_json::from_slice(&config_bytes)?;
        let mut tokenizer = Tokenizer::from_bytes(&tokenizer_bytes)
            .map_err(|e| anyhow::anyhow!("tokenizer: {e}"))?;
        tokenizer.with_padding(None);
        tokenizer
            .with_truncation(Some(TruncationParams {
                max_length: 256,
                ..Default::default()
            }))
            .map_err(|e| anyhow::anyhow!("tokenizer truncation: {e}"))?;
        let vb = VarBuilder::from_buffered_safetensors(weights, DTYPE, &Device::Cpu)?;
        let model = BertModel::load(vb, &config)?;
        let pool = rayon::ThreadPoolBuilder::new()
            .num_threads(2)
            .thread_name(|index| format!("curator-embed-{index}"))
            .build()?;
        Ok(Self {
            model,
            tokenizer,
            pool,
        })
    }

    fn encode(&self, text: &str) -> Result<Vec<f32>> {
        self.pool.install(|| self.encode_here(text))
    }

    fn encode_here(&self, text: &str) -> Result<Vec<f32>> {
        let tokens = self
            .tokenizer
            .encode(text, true)
            .map_err(|e| anyhow::anyhow!("encode: {e}"))?;
        let ids = Tensor::new(tokens.get_ids(), &Device::Cpu)?.unsqueeze(0)?;
        let mut values = self
            .model
            .forward(&ids, &ids.zeros_like()?, None)?
            .mean(1)?
            .squeeze(0)?
            .to_vec1::<f32>()?;
        if values.len() != DIM || values.iter().any(|v| !v.is_finite()) {
            bail!("invalid embedding dimension or value");
        }
        let norm = values.iter().map(|v| v * v).sum::<f32>().sqrt();
        if !norm.is_finite() || norm <= 0.0 {
            bail!("invalid embedding norm");
        }
        values.iter_mut().for_each(|v| *v /= norm);
        Ok(values)
    }
}

fn valid_request(request: &Request) -> bool {
    request.protocol_version == 1
        && request.model_id == MODEL_ID
        && !request.request_id.is_empty()
        && request.request_id.len() <= 64
        && request
            .request_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-')
        && !request.texts.is_empty()
        && request.texts.len() <= MAX_BATCH
        && request
            .texts
            .iter()
            .all(|s| !s.is_empty() && s.len() <= MAX_TEXT_BYTES)
}

fn response_for<'a>(encoder: &Encoder, request: &'a Request) -> Response<'a> {
    if !valid_request(request) {
        return Response {
            protocol_version: 1,
            request_id: &request.request_id,
            model_id: MODEL_ID,
            vectors: None,
            error: Some("bad_request"),
        };
    }
    let mut vectors = Vec::with_capacity(request.texts.len());
    for text in &request.texts {
        match encoder.encode(text) {
            Ok(vector) => vectors.push(vector),
            Err(_) => {
                return Response {
                    protocol_version: 1,
                    request_id: &request.request_id,
                    model_id: MODEL_ID,
                    vectors: None,
                    error: Some("encode_failed"),
                };
            }
        }
    }
    Response {
        protocol_version: 1,
        request_id: &request.request_id,
        model_id: MODEL_ID,
        vectors: Some(vectors),
        error: None,
    }
}

fn model_dir() -> Result<PathBuf> {
    let mut args = std::env::args().skip(1);
    match (args.next().as_deref(), args.next(), args.next()) {
        (Some("--model-dir"), Some(path), None) => Ok(PathBuf::from(path)),
        _ => bail!("usage: curator-embed --model-dir DIR"),
    }
}

fn run() -> Result<()> {
    let encoder = Encoder::load(&model_dir()?)?;
    let stdin = io::stdin();
    let mut input = stdin.lock();
    let stdout = io::stdout();
    let mut output = stdout.lock();
    let mut line = Vec::new();
    loop {
        line.clear();
        if read_bounded_line(&mut input, &mut line)? == 0 {
            return Ok(());
        }
        let request: Request = serde_json::from_slice(&line).context("invalid protocol input")?;
        let response = response_for(&encoder, &request);
        let response_bytes = serde_json::to_vec(&response)?;
        if response_bytes.len() + 1 > 256 * 1024 {
            bail!("protocol output exceeds limit");
        }
        output.write_all(&response_bytes)?;
        output.write_all(b"\n")?;
        output.flush()?;
    }
}

fn read_bounded_line(input: &mut impl BufRead, line: &mut Vec<u8>) -> Result<usize> {
    loop {
        let available = input.fill_buf()?;
        if available.is_empty() {
            return Ok(line.len());
        }
        let take = available
            .iter()
            .position(|&byte| byte == b'\n')
            .map_or(available.len(), |n| n + 1);
        if line.len() + take > MAX_LINE_BYTES {
            bail!("protocol input exceeds limit");
        }
        line.extend_from_slice(&available[..take]);
        input.consume(take);
        if line.last() == Some(&b'\n') {
            return Ok(line.len());
        }
    }
}

fn main() {
    if let Err(error) = run() {
        eprintln!("curator-embed: {error:#}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_limits_are_explicit() {
        let mut request = Request {
            protocol_version: 1,
            request_id: "req-1".into(),
            model_id: MODEL_ID.into(),
            texts: vec!["gay-themed TV series".into()],
        };
        assert!(valid_request(&request));
        request.texts = vec!["x".repeat(MAX_TEXT_BYTES + 1)];
        assert!(!valid_request(&request));
        request.texts = vec!["okay".into()];
        request.model_id = "other-model".into();
        assert!(!valid_request(&request));
    }

    #[test]
    fn line_reader_stops_at_limit() {
        let mut input = std::io::Cursor::new(b"one\ntwo\n".to_vec());
        let mut line = Vec::new();
        assert_eq!(read_bounded_line(&mut input, &mut line).unwrap(), 4);
        assert_eq!(line, b"one\n");
        line.clear();
        assert_eq!(read_bounded_line(&mut input, &mut line).unwrap(), 4);
        assert_eq!(line, b"two\n");
        let mut input = std::io::Cursor::new(vec![b'x'; MAX_LINE_BYTES + 1]);
        line.clear();
        assert!(read_bounded_line(&mut input, &mut line).is_err());
        assert!(line.len() <= MAX_LINE_BYTES);
    }

    #[test]
    fn largest_decoded_batch_fits_transport_envelope() {
        let request = Request {
            protocol_version: 1,
            request_id: "r".repeat(64),
            model_id: MODEL_ID.into(),
            texts: vec!["\0".repeat(MAX_TEXT_BYTES); MAX_BATCH],
        };
        assert!(valid_request(&request));
        let mut serialized = serde_json::to_vec(&request).unwrap();
        serialized.push(b'\n');
        assert!(serialized.len() <= MAX_LINE_BYTES);
        let mut line = Vec::new();
        assert_eq!(
            read_bounded_line(&mut std::io::Cursor::new(serialized.clone()), &mut line).unwrap(),
            serialized.len()
        );
        let parsed: Request = serde_json::from_slice(&line).unwrap();
        assert!(valid_request(&parsed));
        assert_eq!(parsed.texts, request.texts);
        let oversized = vec![b'x'; MAX_LINE_BYTES + 1];
        assert!(read_bounded_line(&mut std::io::Cursor::new(oversized), &mut Vec::new()).is_err());
    }

    #[test]
    fn verified_read_rejects_oversize_and_corruption() {
        let expected = format!("{:x}", Sha256::digest(b"good"));
        assert_eq!(
            read_verified(std::io::Cursor::new(b"good"), 4, &expected).unwrap(),
            b"good"
        );
        assert!(read_verified(std::io::Cursor::new(b"goods"), 4, &expected).is_err());
        assert!(read_verified(std::io::Cursor::new(b"bad!"), 4, &expected).is_err());
    }

    // Recorded by Cinema's onig tokenizer harness. Run with
    // CURATOR_EMBED_MODEL_DIR=/path/to/verified/model cargo test -- --ignored.
    #[test]
    #[ignore = "requires the pinned tokenizer files"]
    fn tokenizer_ids_match_cinema_fixture() {
        let dir = PathBuf::from(std::env::var("CURATOR_EMBED_MODEL_DIR").expect("model directory"));
        let bytes = verified_file(&dir, FILES[1].0, FILES[1].1, FILES[1].2).unwrap();
        let mut tokenizer = Tokenizer::from_bytes(&bytes).unwrap();
        tokenizer.with_padding(None);
        tokenizer
            .with_truncation(Some(TruncationParams {
                max_length: 256,
                ..Default::default()
            }))
            .unwrap();
        let inputs: Vec<_> = include_str!("../testdata/tokenizer_corpus.txt")
            .lines()
            .filter(|line| !line.starts_with("# "))
            .collect();
        let recorded: Vec<_> = include_str!("../testdata/tokenizer_corpus.ids")
            .lines()
            .take(inputs.len())
            .collect();
        assert_eq!(recorded.len(), inputs.len());
        for (index, (input, expected)) in inputs.iter().zip(recorded).enumerate() {
            let ids = tokenizer
                .encode(*input, true)
                .unwrap()
                .get_ids()
                .iter()
                .map(u32::to_string)
                .collect::<Vec<_>>()
                .join(" ");
            assert_eq!(ids, expected, "fixture input {index}");
        }
        println!("matched {} Cinema tokenizer inputs", inputs.len());
    }

    #[test]
    #[ignore = "requires the pinned model files"]
    fn vectors_match_independent_pytorch_fixture() {
        let dir = PathBuf::from(std::env::var("CURATOR_EMBED_MODEL_DIR").expect("model directory"));
        let encoder = Encoder::load(&dir).unwrap();
        let fixture: serde_json::Value =
            serde_json::from_str(include_str!("../testdata/reference_vectors.json")).unwrap();
        assert_eq!(
            fixture["model_revision"],
            "1110a243fdf4706b3f48f1d95db1a4f5529b4d41"
        );
        for (index, case) in fixture["cases"].as_array().unwrap().iter().enumerate() {
            let text = case["base"]
                .as_str()
                .unwrap()
                .repeat(case["repeat"].as_u64().unwrap() as usize);
            let ids = encoder.tokenizer.encode(text.as_str(), true).unwrap();
            assert_eq!(ids.len(), case["token_count"].as_u64().unwrap() as usize);
            let actual = encoder.encode(&text).unwrap();
            let expected = case["vector"].as_array().unwrap();
            assert_eq!(actual.len(), expected.len());
            let max_abs = actual
                .iter()
                .zip(expected)
                .map(|(a, b)| (f64::from(*a) - b.as_f64().unwrap()).abs())
                .fold(0.0_f64, f64::max);
            assert!(max_abs < 1e-5, "case {index}: maximum error {max_abs}");
            println!("case {index}: max absolute vector error {max_abs:.9}");
        }
    }
}
