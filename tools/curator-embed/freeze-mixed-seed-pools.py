"""Freeze M0d mixed seed candidate identities without labels or model scores."""

import argparse
import importlib.util
import json
from pathlib import Path

from seed_topic import evidence_topics

ROOT = Path(__file__).resolve().parents[2]
SOURCE_PRIORITY = {
    "queer": ("lgbt", "boys' love (bl)", "gay theme", "gay romance", "gay relationship"),
    "space_exploration": ("space travel", "space exploration", "spacecraft", "outer space"),
    "political_drama": ("politics", "political drama", "political corruption"),
    "coming_of_age": ("coming of age", "adolescence", "growing up"),
    "found_family": ("found family", "chosen family"),
}
TOPIC_ORDER = tuple(SOURCE_PRIORITY)
CAPTURED_PAGE = {
    "lgbt": ("extra", "lgbt", 158718),
    "boys' love (bl)": ("r3", "bl", 289844),
    "space travel": ("extra", "space_travel", 3801),
    "politics": ("extra", "politics", 6078),
}


def load(path):
    return json.loads(path.read_text())


def module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--details", type=Path, required=True)
    parser.add_argument("--looking-pages", type=Path, required=True)
    parser.add_argument("--extra-pages", type=Path, required=True)
    parser.add_argument("--r3-pages", type=Path, required=True)
    parser.add_argument("--aux-pages", type=Path, required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "docs/smart-media-discovery-m0d-mixed-candidates.json")
    args = parser.parse_args()
    freeze = module(ROOT / "tools/curator-embed/freeze-corpus-candidates.py", "freeze")
    rows = {row["id"]: row for row in load(args.details)["rows"]}
    looking = load(args.looking_pages)["summaries"]
    extra = {page["name"]: page for page in load(args.extra_pages)["rows"]}
    r3 = {page["lane"]: page for page in load(args.r3_pages)["rows"] if page["filter"] == "unfiltered"}
    aux = load(args.aux_pages)["rows"]

    def related_pages(seed):
        if seed == 57774:
            return looking
        if seed == 122009:
            return [extra[f"{seed}_recommendations_1"], extra[f"{seed}_similar_1"],
                    extra[f"{seed}_recommendations_2"]]
        return [next(page for page in aux if page["seed_id"] == seed and
                     page["kind"] == kind and page["page"] == number)
                for kind, number in (("recommendations", 1), ("similar", 1), ("recommendations", 2))]

    captured = []
    for seed in (57774, 122009, 655, 1425):
        pages = related_pages(seed)
        related = freeze.unique(freeze.interleave_pages(pages[:2]) +
                                [row["id"] for row in pages[2]["results"]])
        related = [id for id in related if id != seed][:60]
        topics = evidence_topics(rows[seed])
        source = []
        for topic in TOPIC_ORDER:
            if topic not in topics or len(source) == 2:
                continue
            for alias in SOURCE_PRIORITY[topic]:
                if alias not in {name.casefold() for name in rows[seed]["keywords"]}:
                    continue
                if alias not in CAPTURED_PAGE:
                    break  # The chosen alias's page was not captured; do not substitute.
                location, name, keyword_id = CAPTURED_PAGE[alias]
                page = extra[name] if location == "extra" else r3[name]
                recorded_id = (page.get("params", {}).get("with_keywords") if location == "extra"
                               else page["keyword_id"])
                assert recorded_id == keyword_id and page["status"] == 200
                source.append({"topic": topic, "keyword_name": alias,
                               "keyword_id": keyword_id, "page_name": name,
                               "page_result_ids": [row["id"] for row in page["results"]]})
                break
        assert len(source) == 1, (seed, source, topics)
        related_quota = 40 if len(source) == 1 else 20
        retained = [id for id in related[:related_quota]]
        provenance = {id: ["related"] for id in retained}
        for page in source:
            for id in page["page_result_ids"][:20]:
                if id == seed:
                    continue
                provenance.setdefault(id, []).append("topic:" + page["keyword_name"])
                if id not in retained:
                    retained.append(id)
        for id in related[related_quota:]:
            if len(retained) == 60:
                break
            if id not in retained:
                retained.append(id)
                provenance.setdefault(id, []).append("related_fill")
        assert len(retained) <= 60 and len(retained) == len(set(retained)) and seed not in retained
        new_ids = [id for id in retained if id not in related]
        captured.append({"seed_tmdb_id": seed, "seed_name": rows[seed]["name"],
                         "recognized_topics": [topic for topic in TOPIC_ORDER if topic in topics],
                         "source_pages": source, "related_pool_ids": related,
                         "related_quota": related_quota, "mixed_pool_ids": retained,
                         "topic_new_to_related_ids": new_ids,
                         "missing_detail_ids": [id for id in retained if id not in rows],
                         "candidate_sources": {str(id): provenance[id] for id in retained}})
    output = {"version": "m0d-mixed-candidate-freeze-v1",
              "status": "identities and source provenance only; no labels or model ranks read",
              "source_rule": "one verified seed keyword page; related40+topic20; exact seed-ID exclusion; dedup then related fill to60",
              "queries": captured,
              "new_tuning_query_wordings": [
                  {"id": "N01", "seed_tmdb_id": 122009,
                   "prose": "romantic rivals at university like Bad Buddy", "filters": {}},
                  {"id": "N02", "seed_tmdb_id": 1425,
                   "prose": "political corruption dramas like House of Cards", "filters": {}},
              ]}
    args.output.write_text(json.dumps(output, indent=2) + "\n")
    for row in captured:
        print(row["seed_tmdb_id"], "related", len(row["related_pool_ids"]),
              "mixed", len(row["mixed_pool_ids"]), "new", len(row["topic_new_to_related_ids"]),
              "missing_details", len(row["missing_detail_ids"]))


if __name__ == "__main__":
    main()
