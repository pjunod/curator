"""M0c tuning-only ranking probe over previously annotated R3/Looking IDs.

Full TMDB detail text is a temporary input, never written to the repository.
Labels are read only by the reporting path; ranking reads provider metadata.
This is not the production retrieval, evidence, or concurrency implementation.
"""

import argparse
import json
import math
import pathlib
import re
import subprocess
import time


ROOT = pathlib.Path(__file__).resolve().parents[2]
NARROW_KEYWORDS = {"gay romance", "gay relationship", "gay theme", "boys' love (bl)", "gay"}
NARROW_OVERVIEW = re.compile(r"\bgay (?:man|men|boy|boys|guy|couple)\b|\btwo (?:men|boys)\b.{0,90}\b(?:love|romance|relationship)\b", re.I)
QUERY = "Stories about gay men and relationships between men"


def labels_from_markdown() -> dict[str, dict[int, int]]:
    text = (ROOT / "docs/smart-media-discovery-m0a-annotations.md").read_text()
    sections = {
        "R3": text.split("## 3. R3 theme candidates", 1)[1].split("## 4. Looking seed candidates", 1)[0],
        "R4_57774": text.split("## 4. Looking seed candidates", 1)[1].split("## 5. Adjudication", 1)[0],
    }
    result = {}
    for route, section in sections.items():
        grades = {}
        for line in section.splitlines():
            cells = [cell.strip() for cell in line.split("|")]
            if len(cells) < 8 or not cells[1].isdigit():
                continue
            match = re.search(r"\)\s+(\d+)$", cells[2])
            if match:
                grades[int(match.group(1))] = int(cells[6].rstrip("*"))
        assert len(grades) == 30, (route, len(grades))
        result[route] = grades
    return result


def rank_contributions(fixture: dict, route: str) -> dict[int, float]:
    lists = (
        [(name, 0) for name in ("R3_gay_theme", "R3_BL", "R3_lgbt")]
        if route == "R3"
        else [("R4_57774_recommendations1", 0), ("R4_57774_recommendations2", 20), ("R4_57774_similar1", 0)]
    )
    ranks: dict[int, dict[str, int]] = {}
    for name, offset in lists:
        list_id = name.replace("recommendations1", "recommendations").replace("recommendations2", "recommendations").replace("similar1", "similar")
        rows = fixture["pages"][name]["pages"][0]["rows"]
        for row_index, row in enumerate(rows, 1):
            rank = offset + row_index
            rank_by_list = ranks.setdefault(row["id"], {})
            rank_by_list[list_id] = min(rank, rank_by_list.get(list_id, rank))
    return {id: sum(1 / (60 + rank) for rank in by_list.values()) for id, by_list in ranks.items()}


def eligible_theme(row: dict) -> bool:
    keywords = {name.casefold() for name in row["keywords"]}
    return bool(keywords & NARROW_KEYWORDS) or bool(NARROW_OVERVIEW.search(row["overview"]))


def metadata_text(row: dict) -> str:
    # Evidence-bearing fields precede plot. No title, cast, votes or labels.
    keywords = sorted(row["keywords"], key=str.casefold)[:16]
    return "Keywords: " + ", ".join(keywords) + ". Genres: " + ", ".join(row["genres"]) + ". Synopsis: " + row["overview"]


def encode(helper: pathlib.Path, model_dir: pathlib.Path, texts: list[str]) -> tuple[list[list[float]], float]:
    source = (ROOT / "tools/curator-embed/src/main.rs").read_text()
    model_id = re.search(r'const MODEL_ID: &str = "([^"]+)', source).group(1)
    requests = [
        {"protocol_version": 1, "request_id": f"probe-{index}", "model_id": model_id, "texts": texts[index:index + 8]}
        for index in range(0, len(texts), 8)
    ]
    payload = "".join(json.dumps(request) + "\n" for request in requests)
    start = time.monotonic()
    run = subprocess.run([str(helper), "--model-dir", str(model_dir)], input=payload, capture_output=True, text=True, check=True, timeout=60)
    elapsed = time.monotonic() - start
    responses = [json.loads(line) for line in run.stdout.splitlines()]
    assert len(responses) == len(requests)
    vectors = []
    for request, response in zip(requests, responses):
        assert response["protocol_version"] == 1 and response["request_id"] == request["request_id"]
        assert response["model_id"] == model_id and response["error"] is None
        assert len(response["vectors"]) == len(request["texts"])
        for vector in response["vectors"]:
            assert len(vector) == 384 and all(math.isfinite(value) for value in vector)
            vectors.append(vector)
    return vectors, elapsed


def ndcg(order: list[int], grades: dict[int, int]) -> float:
    def dcg(values):
        return sum((2 ** grade - 1) / math.log2(rank + 2) for rank, grade in enumerate(values[:10]))
    ideal = dcg(sorted(grades.values(), reverse=True))
    return dcg([grades[id] for id in order]) / ideal if ideal else 0.0


def report(route: str, rows: dict[int, dict], ids: list[int], grades: dict[int, int], ranks: dict[int, float], vectors: dict[int, list[float]], query_vector: list[float]) -> None:
    eligible = [id for id in ids if route != "R3" or eligible_theme(rows[id])]
    assert set(eligible) <= set(grades)
    base = sorted(eligible, key=lambda id: (-ranks[id], id))
    semantic = sorted(eligible, key=lambda id: (-sum(a*b for a, b in zip(vectors[id], query_vector)), -ranks[id], id))
    print(route, "selected", len(ids), "eligible", len(eligible), "suitable", sum(grades[id] >= 2 for id in eligible))
    for name, order in (("metadata", base), ("semantic", semantic)):
        top = order[:10]
        precision = sum(grades[id] >= 2 for id in top) / len(top) if top else 0.0
        print(name, "P@10", f"{precision:.3f}", "nDCG@10", f"{ndcg(order, {id: grades[id] for id in eligible}):.4f}")
        for index, id in enumerate(top, 1):
            print(f"  {index:2} {id:6} grade={grades[id]} {rows[id]['name']}")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--details", type=pathlib.Path, required=True)
    parser.add_argument("--model-dir", type=pathlib.Path, required=True)
    parser.add_argument("--helper", type=pathlib.Path, default=ROOT / "tools/curator-embed/target/release/curator-embed")
    args = parser.parse_args()
    details = json.loads(args.details.read_text())["rows"]
    assert all(row["status"] == 200 for row in details)
    rows = {row["id"]: row for row in details}
    fixture = json.loads((ROOT / "docs/smart-media-discovery-m0a-revision-fixture.json").read_text())
    labels = labels_from_markdown()
    route_ids = {route: fixture["routes"][route]["preselected30_ids"] for route in ("R3", "R4_57774")}
    assert all(set(ids) == set(labels[route]) for route, ids in route_ids.items())
    candidate_ids = sorted(set().union(*map(set, route_ids.values())))
    seed = rows[57774]
    texts = [QUERY, metadata_text(seed)] + [metadata_text(rows[id]) for id in candidate_ids]
    vectors, elapsed = encode(args.helper, args.model_dir, texts)
    print("encoder_inputs", len(texts), "elapsed_s", f"{elapsed:.3f}", "text_format", "m0c-probe-v1")
    by_id = dict(zip(candidate_ids, vectors[2:]))
    for route in route_ids:
        report(route, rows, route_ids[route], labels[route], rank_contributions(fixture, route), by_id, vectors[0 if route == "R3" else 1])


if __name__ == "__main__":
    main()
