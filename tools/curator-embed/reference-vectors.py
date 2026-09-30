"""Generate independent PyTorch reference vectors for the pinned MiniLM files.

Install requirements-reference.txt into a temporary Python 3.12 environment.
Run with a local verified model directory and an output JSON path. This script
uses Transformers' BertModel, not the Rust/Candle encoder under test.
"""

import hashlib
import json
import os
import pathlib
import sys

os.environ["HF_HUB_OFFLINE"] = "1"
os.environ["TRANSFORMERS_OFFLINE"] = "1"

import torch
from tokenizers import Tokenizer
from transformers import BertModel


def main() -> None:
    model_dir = pathlib.Path(sys.argv[1])
    output = pathlib.Path(sys.argv[2])
    manifest = json.loads((pathlib.Path(__file__).parent / "model-manifest.json").read_text())
    for name, expected in manifest["files"].items():
        data = (model_dir / name).read_bytes()
        assert len(data) == expected["size"], name
        assert hashlib.sha256(data).hexdigest() == expected["sha256"], name

    torch.set_num_threads(2)
    model = BertModel.from_pretrained(model_dir, local_files_only=True).eval()
    tokenizer = Tokenizer.from_file(str(model_dir / "tokenizer.json"))
    tokenizer.no_padding()
    tokenizer.enable_truncation(max_length=256)
    cases = [
        ("A tender coming-of-age gay romance between two teenage boys.", 1),
        ("A gritty crime series set in London.", 1),
        ("短い物語 — café and family.", 1),
        ("A long story about friendship, identity, and love. ", 100),
        ("Young Royals", 1),
    ]
    result = {
        "model_revision": manifest["revision"],
        "reference": "torch-2.5.1-transformers-4.46.3-tokenizers-0.20.3",
        "pooling": "unmasked-token-mean-l2",
        "cases": [],
    }
    for base, repeat in cases:
        ids = tokenizer.encode(base * repeat).ids
        with torch.no_grad():
            token_ids = torch.tensor([ids], dtype=torch.long)
            hidden = model(input_ids=token_ids, token_type_ids=torch.zeros_like(token_ids)).last_hidden_state
            pooled = hidden.mean(dim=1).squeeze(0)
            vector = torch.nn.functional.normalize(pooled, dim=0).tolist()
        result["cases"].append({"base": base, "repeat": repeat, "token_count": len(ids), "vector": vector})
    output.write_text(json.dumps(result, ensure_ascii=False, separators=(",", ":")) + "\n")


if __name__ == "__main__":
    main()
