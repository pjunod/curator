"""Freeze two non-LGBTQ auxiliary tuning seeds before model ordering."""

import argparse
import json
from pathlib import Path

from seed_topic import infer_seed_topic


ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--pages", type=Path, required=True)
    parser.add_argument("--details", type=Path, required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "docs/smart-media-discovery-m0c-aux-seeds.json")
    args = parser.parse_args()
    pages = json.loads(args.pages.read_text())["rows"]
    details = {row["id"]: row for row in json.loads(args.details.read_text())["rows"]}
    assert all(page["status"] == 200 for page in pages)
    queries = []
    for query_id, seed_id, prose, theme in (
        ("A01", 655, "shows like Star Trek: The Next Generation", "space_exploration"),
        ("A02", 1425, "shows like House of Cards", "political_drama"),
    ):
        source = [next(page for page in pages if page["seed_id"] == seed_id and
                  page["kind"] == kind and page["page"] == number)
                  for kind, number in (("recommendations", 1), ("similar", 1), ("recommendations", 2))]
        first = [page["results"] for page in source[:2]]
        merged = []
        for index in range(max(map(len, first))):
            for lane in first:
                if index < len(lane) and lane[index]["id"] != seed_id and lane[index]["id"] not in merged:
                    merged.append(lane[index]["id"])
        for row in source[2]["results"]:
            if row["id"] != seed_id and row["id"] not in merged:
                merged.append(row["id"])
        merged = merged[:60]
        assert infer_seed_topic(details[seed_id]) == ("space_exploration" if seed_id == 655 else "political_drama")
        queries.append({"query_id": query_id, "split": "auxiliary_tuning", "prose": prose,
                        "seed_tmdb_id": seed_id, "expected_seed_topic": theme,
                        "pool_ids": merged, "pool_count": len(merged)})
    output = {"version": "m0c-aux-seeds-v1", "frozen_local_date": "2026-09-29",
              "status": "query intent and candidate IDs frozen before provisional grades or embedding order",
              "purpose": "test provider-derived topic selection beyond queer seeds; not added to the frozen 24-query held-out split",
              "queries": queries}
    args.output.write_text(json.dumps(output, indent=2) + "\n")
    print([(q["query_id"], q["pool_count"]) for q in queries])


if __name__ == "__main__":
    main()
