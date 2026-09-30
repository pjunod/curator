"""Provider-only soft topic signals and generic embedding text for M0c.

These signals never establish required-theme admission or centrality. A seed
with multiple or unrecognized topics still has usable semantic metadata; its
topic bonus is zero. Only a genuinely thin seed follows provider-only ranking.
"""

import re

MAX_TEXT_BYTES = 8192


def utf8_prefix(value, limit):
    """Return the longest valid UTF-8 prefix no longer than limit bytes."""
    assert limit >= 0
    return value.encode("utf-8")[:limit].decode("utf-8", "ignore")


def summary_text(overview):
    stripped = overview.strip()
    return utf8_prefix(stripped, MAX_TEXT_BYTES) if stripped else None

ALIASES = {
    "queer": ("lgbt", "lgbt+", "lgbtq", "gay theme", "gay romance", "gay relationship",
              "boys' love (bl)", "lesbian", "lesbian relationship", "lesbian romance",
              "girls' love (gl)", "queer", "transgender", "bisexuality"),
    "space_exploration": ("space exploration", "space travel", "outer space", "space", "astronaut",
                          "starship", "spacecraft", "galaxy", "planet"),
    "political_drama": ("politics", "political drama", "political intrigue", "government conspiracy",
                        "political corruption", "usa president", "prime minister", "parliament", "election"),
    "coming_of_age": ("coming of age", "adolescence", "growing up", "teen-coming-of-age stories"),
    "found_family": ("found family", "chosen family"),
}
CONTEXT_KEYWORDS = (
    "male friendship", "female friendship", "friendship", "friends to lovers",
    "rivals to lovers", "enemies to lovers", "university", "family relationships",
    "coming out", "romance", "relationship", "exploration", "space opera",
)
OVERVIEW = {
    "queer": re.compile(r"\b(?:gay (?:man|men|couple|life)|lesbian (?:woman|women|couple|relationship)|"
                        r"queer (?:person|people|life|romance|relationship)|lgbtq? (?:people|life|community)|"
                        r"same.sex (?:couple|relationship|romance)|boys.? love|girls.? love)\b", re.I),
    "space_exploration": re.compile(r"\b(?:outer space|spacecraft|starships?|astronauts?|interstellar|"
                                     r"galax(?:y|ies)|space mission|explor\w* (?:new )?(?:worlds|planets))\b", re.I),
    "political_drama": re.compile(r"\b(?:politic\w*|government|parliament|prime minister|"
                                  r"presidential election|president of (?:the )?(?:united states|country)|"
                                  r"secretary of state|white house|senat\w*|diplomac\w*)\b", re.I),
    "coming_of_age": re.compile(r"\b(?:coming.of.age|growing up|journey through adolescence)\b", re.I),
    "found_family": re.compile(r"\b(?:found family|chosen family|makeshift family|adoptive family)\b", re.I),
}
ROMANCE = re.compile(r"\b(?:romance|relationship|fall in love|find love|dating|couple)\b", re.I)
SECONDARY = {
    "queer": ROMANCE,
    "space_exploration": re.compile(r"\b(?:explor\w*|mission|voyage)\b", re.I),
    "political_drama": re.compile(r"\b(?:intrigue|power|corrupt\w*)\b", re.I),
    "coming_of_age": re.compile(r"\b(?:young|school|student)\b", re.I),
    "found_family": re.compile(r"\b(?:family|friendship|belong\w*)\b", re.I),
}


def evidence_topics(row):
    keywords = {keyword.casefold() for keyword in row["keywords"]}
    overview = row["overview"]
    return {topic for topic, aliases in ALIASES.items()
            if keywords.intersection(aliases) or OVERVIEW[topic].search(overview)}


def infer_seed_topic(row):
    topics = evidence_topics(row)
    return next(iter(topics)) if len(topics) == 1 else "none"


def summary_topic_score(overview, topic):
    if topic == "none":
        return 0
    return 2 * bool(OVERVIEW[topic].search(overview)) + bool(SECONDARY[topic].search(overview))


def detail_topic_match(row, topic):
    return int(topic != "none" and topic in evidence_topics(row))


def selected_keywords(row, topic):
    keywords = list(dict.fromkeys(row["keywords"]))
    by_name = {keyword.casefold():keyword for keyword in keywords}
    topics = (topic,) if topic in ALIASES else ()
    ordered_topics = topics + tuple(name for name in ALIASES if name not in topics)
    priority = [by_name[alias] for name in ordered_topics for alias in ALIASES[name]
                if alias in by_name]
    priority.extend(by_name[alias] for alias in CONTEXT_KEYWORDS if alias in by_name)
    kept = list(dict.fromkeys(priority))
    kept.extend(keyword for keyword in sorted(keywords,key=str.casefold) if keyword not in kept)
    return kept[:16]


def seed_token_count(row, topic):
    """Count lexical tokens in overview and bounded keywords, excluding title/genres."""
    return len(re.findall(r"\b\w+\b", row["overview"] + " " +
                          " ".join(selected_keywords(row, topic)), re.UNICODE))


def has_usable_seed_semantics(row, topic):
    return seed_token_count(row, topic) >= 8


def provider_text(row, topic):
    """ID-independent text with priority fields inside the helper's byte cap."""
    keywords = [utf8_prefix(value, 128) for value in selected_keywords(row, topic)]
    genres = [utf8_prefix(value, 64) for value in row["genres"][:16]]
    prefix = ("Keywords: " + ", ".join(keywords) + ". Genres: " +
              ", ".join(genres) + ". Synopsis: ")
    assert len(prefix.encode("utf-8")) < MAX_TEXT_BYTES
    available = MAX_TEXT_BYTES - len(prefix.encode("utf-8"))
    return prefix + utf8_prefix(row["overview"], available)


def check_examples():
    examples = (
        ("Two men fall in love with the same woman.", "none"),
        ("Two women investigate a burglary.", "none"),
        ("A family needs more space in their apartment.", "none"),
        ("A president of a school club organizes a picnic.", "none"),
        ("Three friends explore life as gay men.", "queer"),
        ("A crew explores new worlds aboard a starship.", "space_exploration"),
        ("A prime minister survives a political scandal.", "political_drama"),
        ("Astronauts investigate a government election aboard a spacecraft.", "none"),
    )
    for overview, expected in examples:
        assert infer_seed_topic({"overview":overview,"keywords":[]}) == expected
    assert summary_topic_score("A gay couple finds love.","queer") == 3
    assert infer_seed_topic({"overview":"","keywords":["gay romance"]}) == "queer"
    assert not has_usable_seed_semantics({"overview":"","keywords":[]},"none")
    assert has_usable_seed_semantics({"overview":"Three friends find love and work in San Francisco.","keywords":[]},"none")
    assert summary_text("🙂" * 2049) == "🙂" * 2048
    assert summary_text("   ") is None
    long = provider_text({"keywords":["z"*9000,"gay romance"],"genres":["é"*9000],
                          "overview":"🙂"*9000},"queer")
    assert len(long.encode("utf-8")) <= MAX_TEXT_BYTES and "gay romance" in long


check_examples()
