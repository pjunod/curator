"""M0c tuning probe v2: stronger evidence-tier baseline and seed selection.

Reads temporary provider details. Labels affect metrics only, never selection
or ranking. The semantic seed preselector sees overview summaries only; detail
keywords are introduced only after the 30 enrichment places are selected.
"""

import argparse
import importlib.util
import json
import math
import pathlib
import re
import subprocess
import time


ROOT = pathlib.Path(__file__).resolve().parents[2]
NARROW = ("gay romance", "gay relationship", "gay theme", "boys' love (bl)", "gay")
THEME_SIGNALS = NARROW + ("lgbt",)
LOOKING_SIGNALS = ("male friendship", "friendship", "lgbt", "gay theme", "gay man", "gay relationship", "gay romance", "boys' love (bl)")
QUEER_WORDS = re.compile(r"\b(?:lgbt|lgbtq|gay|lesbian|queer|bisexual|transgender|homosexual)\b", re.I)
OVERVIEW_PRESENT = re.compile(r"\bgay (?:man|men|boy|boys|guy|couple)\b|\btwo (?:men|boys)\b.{0,90}\b(?:love|romance|relationship)\s+(?:with\s+)?each other\b", re.I)
CENTRAL_PATTERNS = (
    re.compile(r"^Bullied for being gay,\s+[^,.]+\s+(?:finds|meets|falls)", re.I),
    re.compile(r"^(?:Original )?drama series\b.{0,100}\bexploring\b.{0,80}\bgay life\b", re.I),
    re.compile(r"^.{0,45}\bshow explores\b.{0,110}\bgay couple\b", re.I),
    re.compile(r"^Three friends\b.{0,150}\b(?:generation of gay men|gay life)\b", re.I),
)
QUERY = "Stories about gay men and relationships between men"


def check_evidence_examples() -> None:
    # Adversarial provider-text cases; they must never be promoted to central.
    examples = (
        ("A straight detective questions a gay man while solving an unrelated murder.", ["gay theme"], 1),
        ("Two men fall in love with the same woman.", [], 0),
        ('A detective says "gay life" while investigating unrelated crimes.', ["gay theme"], 1),
        ("A teen denies he is gay.", ["gay theme"], 1),
        ("Original drama series exploring the passions and pitfalls of 21st century gay life.", ["gay theme"], 2),
    )
    for overview, keywords, expected in examples:
        assert evidence_tier({"overview": overview, "keywords": keywords}) == expected


def old_probe():
    spec = importlib.util.spec_from_file_location("probe_ranking_v1", pathlib.Path(__file__).with_name("probe-ranking.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def evidence_tier(row: dict) -> int:
    keywords = {keyword.casefold() for keyword in row["keywords"]}
    overview = row["overview"]
    if not (keywords.intersection(NARROW) or OVERVIEW_PRESENT.search(overview)):
        return 0
    # Strictly anchored main-story claims. Provider keywords alone cannot
    # promote centrality; this remains a tuning proxy, not production proof.
    return 2 if any(pattern.search(overview) for pattern in CENTRAL_PATTERNS) else 1


def detail_text(row: dict, signals: tuple[str, ...]) -> str:
    by_name = {name.casefold(): name for name in row["keywords"]}
    prioritized = [by_name[name] for name in signals if name in by_name]
    used = {name.casefold() for name in prioritized}
    generic = [name for name in sorted(row["keywords"], key=str.casefold) if name.casefold() not in used]
    kept = (prioritized + generic)[:16]
    return "Keywords: " + ", ".join(kept) + ". Genres: " + ", ".join(row["genres"]) + ". Synopsis: " + row["overview"]


def encode(helper: pathlib.Path, model_dir: pathlib.Path, texts: list[str]) -> tuple[list[list[float]], float]:
    source = (ROOT / "tools/curator-embed/src/main.rs").read_text()
    model_id = re.search(r'const MODEL_ID: &str = "([^"]+)', source).group(1)
    requests = [
        {"protocol_version": 1, "request_id": f"v2-{index}", "model_id": model_id, "texts": texts[index:index + 8]}
        for index in range(0, len(texts), 8)
    ]
    start = time.monotonic()
    run = subprocess.run(
        [str(helper), "--model-dir", str(model_dir)],
        input="".join(json.dumps(request) + "\n" for request in requests),
        capture_output=True, text=True, check=True, timeout=60,
    )
    elapsed = time.monotonic() - start
    responses = [json.loads(line) for line in run.stdout.splitlines()]
    assert len(responses) == len(requests)
    vectors = []
    for request, response in zip(requests, responses):
        assert response["protocol_version"] == request["protocol_version"]
        assert response["request_id"] == request["request_id"]
        assert response["model_id"] == request["model_id"] and response["error"] is None
        assert len(response["vectors"]) == len(request["texts"])
        for vector in response["vectors"]:
            assert len(vector) == 384 and all(math.isfinite(value) for value in vector)
            vectors.append(vector)
    return vectors, elapsed


def metrics(order: list[int], grades: dict[int, int]) -> dict:
    k = min(10, len(order))
    relevant = sum(grades[id] >= 2 for id in order[:k])
    def dcg(values):
        return sum((2 ** grade - 1) / math.log2(rank + 2) for rank, grade in enumerate(values[:10]))
    ideal = dcg(sorted(grades.values(), reverse=True))
    return {
        "returned": len(order), "k": k, "relevant_at_k": relevant,
        "precision_at_k": relevant / k if k else None,
        "precision_at_10": relevant / 10 if k == 10 else None,
        "ndcg_at_10": dcg([grades[id] for id in order]) / ideal if ideal else None,
        "zero_ideal": ideal == 0,
    }


def show(label: str, order: list[int], grades: dict[int, int], rows: dict[int, dict]) -> None:
    print(label, metrics(order, {id: grades[id] for id in order}))
    print("  order:", ", ".join(f"{rows[id]['name']} [{id}, g{grades[id]}]" for id in order[:10]))


def main() -> None:
    check_evidence_examples()
    parser = argparse.ArgumentParser()
    parser.add_argument("--details", type=pathlib.Path, required=True)
    parser.add_argument("--summaries", type=pathlib.Path, required=True,
                        help="captured pre-enrichment recommendations/similar pages")
    parser.add_argument("--model-dir", type=pathlib.Path, required=True)
    parser.add_argument("--helper", type=pathlib.Path, default=ROOT / "tools/curator-embed/target/release/curator-embed")
    args = parser.parse_args()
    rows = {row["id"]: row for row in json.loads(args.details.read_text())["rows"]}
    summary_pages = json.loads(args.summaries.read_text())["summaries"]
    assert all(page["status"] == 200 for page in summary_pages)
    summaries = {}
    for page in summary_pages:
        for item in page["results"]:
            summaries.setdefault(item["id"], item)
    assert all(row["status"] == 200 for row in rows.values())
    fixture = json.loads((ROOT / "docs/smart-media-discovery-m0a-revision-fixture.json").read_text())
    old = old_probe()
    grades = old.labels_from_markdown()
    adjudications = json.loads((ROOT / "docs/smart-media-discovery-m0c-adjudications.json").read_text())
    for decision in adjudications["rows"]:
        route, candidate_id = decision["route"], decision["id"]
        assert grades[route][candidate_id] == decision["original_grade"]
        grades[route][candidate_id] = decision["adjudicated_grade"]
    extras = json.loads((ROOT / "docs/smart-media-discovery-m0c-looking-extra-labels.json").read_text())
    seed_grades = grades["R4_57774"] | {row["id"]: row["grade"] for row in extras["rows"]}
    seed_pool = fixture["routes"]["R4_57774"]["pool_ids"]
    assert set(seed_pool) == set(summaries)
    assert set(seed_pool) == set(seed_grades)
    theme_ids = fixture["routes"]["R3"]["preselected30_ids"]
    provider_ids = fixture["routes"]["R4_57774"]["provider30_ids"]
    assert set(theme_ids) <= set(rows) and set(seed_pool) <= set(rows)

    # A separate shallow phase sees only up to 60 provider overview summaries.
    shallow_texts = [detail_text(rows[57774], LOOKING_SIGNALS)] + [summaries[id]["overview"] for id in seed_pool]
    shallow, shallow_seconds = encode(args.helper, args.model_dir, shallow_texts)
    seed_vector = shallow[0]
    shallow_scores = {id: sum(a*b for a,b in zip(seed_vector, vector)) for id, vector in zip(seed_pool, shallow[1:])}
    semantic_selected = sorted(seed_pool, key=lambda id: (-shallow_scores[id], seed_pool.index(id), id))[:30]

    theme_details = sorted(set(theme_ids))
    seed_details = sorted(set(provider_ids) | set(semantic_selected))
    texts = (
        [QUERY, detail_text(rows[57774], LOOKING_SIGNALS)]
        + [detail_text(rows[id], THEME_SIGNALS) for id in theme_details]
        + [detail_text(rows[id], LOOKING_SIGNALS) for id in seed_details]
    )
    deep, deep_seconds = encode(args.helper, args.model_dir, texts)
    by_theme = dict(zip(theme_details, deep[2:2 + len(theme_details)]))
    by_seed = dict(zip(seed_details, deep[2 + len(theme_details):]))
    ranks_theme = old.rank_contributions(fixture, "R3")
    ranks_seed = old.rank_contributions(fixture, "R4_57774")
    print("versions retrieval-r3-v1 evidence-proxy-v2 text-priority-v2 seed-preselect-v1")
    print("summary_detail_overview_equal", sum(summaries[id]["overview"] == rows[id]["overview"] for id in seed_pool),
          "of", len(seed_pool))
    print("encoder shallow_texts", len(shallow_texts), "seconds", round(shallow_seconds, 3),
          "detail_texts", len(texts), "seconds", round(deep_seconds, 3),
          "total_seconds", round(shallow_seconds + deep_seconds, 3))
    eligible = [id for id in theme_ids if evidence_tier(rows[id])]
    tier_counts = {tier: sum(evidence_tier(rows[id]) == tier for id in theme_ids) for tier in (0,1,2)}
    print("R3 selected", len(theme_ids), "eligible", len(eligible), "tiers", tier_counts,
          "suitable", sum(grades["R3"][id] >= 2 for id in eligible))
    base = sorted(eligible, key=lambda id: (-evidence_tier(rows[id]), -ranks_theme[id], id))
    semantic = sorted(eligible, key=lambda id: (-evidence_tier(rows[id]), -sum(a*b for a,b in zip(deep[0], by_theme[id])), -ranks_theme[id], id))
    show("R3 metadata", base, grades["R3"], rows)
    show("R3 semantic", semantic, grades["R3"], rows)

    for label, ids in (("provider30", provider_ids), ("semantic_shallow30", semantic_selected)):
        print("Looking", label, "selected", len(ids), "suitable", sum(seed_grades[id] >= 2 for id in ids))
        base = sorted(ids, key=lambda id: (-ranks_seed[id], id))
        semantic = sorted(ids, key=lambda id: (-sum(a*b for a,b in zip(deep[1], by_seed[id])), -ranks_seed[id], id))
        show("Looking " + label + " metadata", base, seed_grades, rows)
        show("Looking " + label + " semantic", semantic, seed_grades, rows)
        print("Looking", label, "ids", ids)
        # A seed with a provider LGBTQ tag may give a bounded *soft* topic
        # affinity to candidates with explicit queer metadata. This is a
        # tuning scan, not a required-theme gate or a frozen score.
        for bonus in (0.02, 0.04, 0.06, 0.08):
            order = sorted(
                ids,
                key=lambda id: (
                    -(
                        sum(a*b for a, b in zip(deep[1], by_seed[id]))
                        + bonus * bool(QUEER_WORDS.search(" ".join(rows[id]["keywords"]) + " " + rows[id]["overview"]))
                    ),
                    -ranks_seed[id], id,
                ),
            )
            print("Looking", label, "soft_queer_bonus", bonus, metrics(order, {id: seed_grades[id] for id in ids}),
                  "top10_ids", order[:10])


if __name__ == "__main__":
    main()
