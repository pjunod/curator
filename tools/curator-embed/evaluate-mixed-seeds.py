"""Compare frozen M0d mixed versus related-only pools on tuning seeds only."""

import argparse
import importlib.util
import json
import math
from pathlib import Path

from seed_topic import (detail_topic_match, infer_seed_topic, provider_text,
                        summary_text)

ROOT = Path(__file__).resolve().parents[2]


def load(path):
    return json.loads(path.read_text())


def module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


def dot(a, b):
    return sum(x * y for x, y in zip(a, b))


def dcg(grades):
    return sum((2 ** grade - 1) / math.log2(i + 2)
               for i, grade in enumerate(grades[:10]))


def metric(order, grades, selected, common):
    top = order[:20]
    observed = dcg([grades[id] for id in top])
    selected_ideal = dcg(sorted((grades[id] for id in selected), reverse=True))
    common_ideal = dcg(sorted((grades[id] for id in common), reverse=True))
    return {
        "returned": len(top), "suitable_at_10": sum(grades[id] >= 2 for id in top[:10]),
        "p_at_10": round(sum(grades[id] >= 2 for id in top[:10]) / 10, 4)
        if len(top) >= 10 else None,
        "ndcg_selected": round(observed / selected_ideal, 4) if selected_ideal else None,
        "ndcg_common_union": round(observed / common_ideal, 4) if common_ideal else None,
        "top10_ids": top[:10],
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--details", type=Path, required=True)
    parser.add_argument("--looking-pages", type=Path, required=True)
    parser.add_argument("--extra-pages", type=Path, required=True)
    parser.add_argument("--r3-pages", type=Path, required=True)
    parser.add_argument("--aux-pages", type=Path, required=True)
    parser.add_argument("--model-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args()
    rows = {row["id"]: row for row in load(args.details)["rows"]}
    looking = load(args.looking_pages)["summaries"]
    extra = {page["name"]: page for page in load(args.extra_pages)["rows"]}
    r3 = {page["lane"]: page for page in load(args.r3_pages)["rows"]
          if page["filter"] == "unfiltered"}
    aux = load(args.aux_pages)["rows"]
    candidates = {q["seed_tmdb_id"]: q for q in
                  load(ROOT / "docs/smart-media-discovery-m0d-mixed-candidates.json")["queries"]}
    mixed_labels = {q["seed_tmdb_id"]: {int(id): grade for id, grade in q["grades"].items()}
                    for q in load(ROOT / "docs/smart-media-discovery-m0d-mixed-labels.json")["queries"]}
    baseline_v2 = {q["query_id"]: {int(id): grade for id, grade in q["grades"].items()}
                   for q in load(ROOT / "docs/smart-media-discovery-m0c-labels-v2.json")["queries"]}
    old = module(ROOT / "tools/curator-embed/probe-ranking.py", "old")
    looking_grades = old.labels_from_markdown()["R4_57774"]
    looking_grades.update({row["id"]: row["grade"] for row in
                           load(ROOT / "docs/smart-media-discovery-m0c-looking-extra-labels.json")["rows"]})
    for decision in load(ROOT / "docs/smart-media-discovery-m0c-adjudications.json")["rows"]:
        if decision["route"] == "R4_57774":
            looking_grades[decision["id"]] = decision["adjudicated_grade"]
    bad_grades = baseline_v2["T05"] | {int(id): grade for id, grade in
                  load(ROOT / "docs/smart-media-discovery-m0c-badbuddy-extra-labels.json")["grades"].items()}
    aux_grades = {q["seed_tmdb_id"]: {int(id): grade for id, grade in q["grades"].items()}
                  for q in load(ROOT / "docs/smart-media-discovery-m0c-aux-seed-labels.json")["queries"].values()}
    base_grades = {57774: looking_grades, 122009: bad_grades,
                   655: aux_grades[655], 1425: aux_grades[1425]}
    ranker = module(ROOT / "tools/curator-embed/probe-ranking-v2.py", "ranker")
    helper = ROOT / "tools/curator-embed/target/release/curator-embed"

    def related_pages(seed):
        if seed == 57774:
            return looking
        if seed == 122009:
            return [extra[f"{seed}_recommendations_1"], extra[f"{seed}_similar_1"],
                    extra[f"{seed}_recommendations_2"]]
        return [next(page for page in aux if page["seed_id"] == seed and
                     page["kind"] == kind and page["page"] == number)
                for kind, number in (("recommendations", 1), ("similar", 1), ("recommendations", 2))]

    for seed, frozen in candidates.items():
        pages = related_pages(seed)
        page_name = frozen["source_pages"][0]["page_name"]
        topic_page = r3[page_name] if page_name == "bl" else extra[page_name]
        summaries = {item["id"]: item for page in pages + [topic_page]
                     for item in page["results"]}
        common = set(frozen["related_pool_ids"] + frozen["mixed_pool_ids"])
        assert common <= set(summaries)
        assert common == set(base_grades[seed] | mixed_labels[seed])
        assert seed not in common
        assert len(frozen["mixed_pool_ids"]) <= 60
        assert all(len(provider_text(rows[id], infer_seed_topic(rows[seed])).encode("utf-8")) <= 8192
                   for id in common if id in rows)
    if args.validate_only:
        print("preflight ok", len(candidates), "tuning seeds")
        return

    output = {"version": "m0d-tuning-mixed-v1",
              "status": "tuning only; mixed identities/grades frozen before first mixed-pool model order",
              "queries": []}
    for seed, frozen in candidates.items():
        topic = infer_seed_topic(rows[seed])
        pages = related_pages(seed)
        related = frozen["related_pool_ids"]
        mixed = frozen["mixed_pool_ids"]
        page_name = frozen["source_pages"][0]["page_name"]
        topic_page = r3[page_name] if page_name == "bl" else extra[page_name]
        summaries = {item["id"]: item for page in pages + [topic_page]
                     for item in page["results"]}
        assert set(related + mixed) <= set(summaries)
        grades = base_grades[seed] | mixed_labels[seed]
        common = list(dict.fromkeys(related + mixed))
        assert set(common) == set(grades), (seed, set(common) ^ set(grades))
        available = [id for id in common if summary_text(summaries[id]["overview"]) is not None]
        shallow_texts = [provider_text(rows[seed], topic)] + [summary_text(summaries[id]["overview"])
                                                         for id in available]
        shallow_vectors, shallow_seconds = ranker.encode(helper, args.model_dir, shallow_texts)
        seed_vector = shallow_vectors[0]
        shallow = {id: dot(seed_vector, vector) for id, vector in
                   zip(available, shallow_vectors[1:])}
        selected = {name: sorted(pool, key=lambda id: (-shallow.get(id, float("-inf")),
                                                       pool.index(id), id))[:30]
                    for name, pool in (("related_only", related), ("mixed", mixed))}
        deep_ids = list(dict.fromkeys(id for selection in selected.values() for id in selection
                                      if id in rows))
        deep_texts = [provider_text(rows[seed], topic)] + [provider_text(rows[id], topic)
                                                    for id in deep_ids]
        deep_vectors, deep_seconds = ranker.encode(helper, args.model_dir, deep_texts)
        cosine = {id: dot(deep_vectors[0], vector) for id, vector in
                  zip(deep_ids, deep_vectors[1:])}
        result = {"seed_tmdb_id": seed, "topic": topic,
                  "related_pool": len(related),
                  "related_pool_suitable": sum(grades[id] >= 2 for id in related),
                  "mixed_pool": len(mixed),
                  "mixed_pool_suitable": sum(grades[id] >= 2 for id in mixed),
                  "common_union": len(common),
                  "shallow_texts": len(shallow_texts),
                  "deep_texts_union": len(deep_texts),
                  "encoder_seconds_total": round(shallow_seconds + deep_seconds, 3)}
        for name, pool in (("related_only", related), ("mixed", mixed)):
            ids = selected[name]
            missing = [id for id in ids if id not in rows]
            eligible = [id for id in ids if id in cosine and cosine[id] >= 0.48]
            order = sorted(eligible, key=lambda id: (-cosine[id] - 0.06 * detail_topic_match(rows[id], topic), id))
            result[name] = {
                "selected_ids": ids,
                "selected_suitable": sum(grades[id] >= 2 for id in ids),
                "selected_missing_detail_ids": missing,
                "eligible": len(eligible),
                "eligible_suitable": sum(grades[id] >= 2 for id in eligible),
                "model": metric(order, grades, ids, common),
            }
        output["queries"].append(result)
        print(seed, "pool suitable", result["related_pool_suitable"],
              result["mixed_pool_suitable"], "selected suitable",
              result["related_only"]["selected_suitable"],
              result["mixed"]["selected_suitable"], "P10",
              result["related_only"]["model"]["p_at_10"],
              result["mixed"]["model"]["p_at_10"])
    args.output.write_text(json.dumps(output, indent=2) + "\n")


if __name__ == "__main__":
    main()
