"""Smoke the final Linux x64 image's non-root helper with a mounted model.

MODEL_DIR is a path on the Docker daemon host. Set DOCKER_HOST for a remote
daemon. The container has no network, uses a read-only root filesystem, and
is removed after the protocol checks.
"""

import argparse
import json
import math
import pathlib
import re
import subprocess
import uuid


def docker(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["docker", *args], capture_output=True, text=True, check=True, timeout=30)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", default="monarr:discovery-m0b")
    parser.add_argument("--model-dir", required=True, help="absolute path on the Docker host")
    args = parser.parse_args()
    model_dir = pathlib.Path(args.model_dir)
    if not model_dir.is_absolute():
        parser.error("--model-dir must be absolute")

    image = docker("image", "inspect", "--format", "{{.Config.User}} {{.Architecture}} {{.Os}}", args.image)
    if image.stdout.strip() != "1000:1000 amd64 linux":
        raise RuntimeError(f"unexpected image user/platform: {image.stdout.strip()}")

    root = pathlib.Path(__file__).parent
    source = (root / "src/main.rs").read_text()
    model_id = re.search(r'const MODEL_ID: &str = "([^"]+)', source).group(1)
    fixture = json.loads((root / "testdata/reference_vectors.json").read_text())
    batches = [
        [case["base"] * case["repeat"] for case in fixture["cases"]],
        ["\0" * 8192] * 8,
    ]
    requests = [
        {"protocol_version": 1, "request_id": f"image-{index}", "model_id": model_id, "texts": batch}
        for index, batch in enumerate(batches)
    ]
    payload = "".join(json.dumps(request) + "\n" for request in requests)
    name = "monarr-m0b-" + uuid.uuid4().hex[:12]
    command = [
        "docker", "run", "--rm", "-i", "--name", name, "--network", "none",
        "--read-only", "--memory", "512m",
        "--mount", f"type=bind,source={model_dir},target=/model,readonly",
        "--entrypoint", "/curator-embed", args.image, "--model-dir", "/model",
    ]
    try:
        result = subprocess.run(command, input=payload, capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise RuntimeError(f"helper exited {result.returncode}: {result.stderr[:500]}")
        responses = [json.loads(line) for line in result.stdout.splitlines()]
        if len(responses) != len(requests):
            raise RuntimeError(f"expected {len(requests)} responses, got {len(responses)}")
        for request, response in zip(requests, responses):
            if (
                response.get("protocol_version") != request["protocol_version"]
                or response.get("request_id") != request["request_id"]
                or response.get("model_id") != request["model_id"]
            ):
                raise RuntimeError("invalid protocol response identity")
            vectors = response.get("vectors")
            if response.get("error") is not None or not isinstance(vectors, list) or len(vectors) != len(request["texts"]):
                raise RuntimeError("invalid helper response")
            for vector in vectors:
                if len(vector) != 384 or not all(math.isfinite(value) for value in vector):
                    raise RuntimeError("invalid vector shape or value")
                norm = math.sqrt(sum(value * value for value in vector))
                if abs(norm - 1.0) >= 1e-4:
                    raise RuntimeError(f"invalid vector norm: {norm}")
        errors = [
            max(abs(actual - expected) for actual, expected in zip(vector, case["vector"]))
            for vector, case in zip(responses[0]["vectors"], fixture["cases"])
        ]
        if max(errors) >= 1e-5:
            raise RuntimeError(f"numeric vector parity failed: {max(errors)}")
        print(f"image={args.image} user=1000:1000 platform=linux/amd64")
        print(f"reference_cases={len(errors)} max_component_error={max(errors):.9g}")
        print(f"max_escape_input_bytes={len((json.dumps(requests[1]) + chr(10)).encode())} vectors={len(responses[1]['vectors'])}")
    finally:
        # --rm normally removes the exited container. A timeout or interrupted
        # Docker client can leave it running, so remove the exact named test.
        subprocess.run(["docker", "rm", "-f", name], capture_output=True, text=True, timeout=30)


if __name__ == "__main__":
    main()
