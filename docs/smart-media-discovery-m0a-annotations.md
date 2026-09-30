# Smart media discovery M0a — candidate evidence annotation

**Status:** rubric frozen before selected-candidate labeling · **Routes:**
[R3 and R4 schedule](smart-media-discovery-m0a-revision.md) at `c78e907` ·
**Reviewer decision:** 2026-09-29 local time, following `3f80a0b`.

Companion to [the discovery plan](plan-smart-media-discovery.md) §6.2 and
[the revised retrieval experiment](smart-media-discovery-m0a-revision.md).
The fixed R3 split-keyword route and fixed Looking seed route each select 30
TMDB identities by the preregistered shallow score. This file annotates
those exact selected identities; it does not replace candidates with titles
from the earlier reference lists. The agent supplies provisional evidence
labels. Human/reviewer adjudication is pending, especially for ambiguous
centrality and seed-fit cases. The
[annotation fixture](smart-media-discovery-m0a-annotation-fixture.json) keeps
every exact provider keyword name for all 59 distinct selected series;
table columns below show only the keywords pertinent to the provisional
theme or seed-fit decision. Provider overviews are paraphrased in the tables.

## 1. Frozen rubric — evidence and suitability are separate

Read each selected `/tv/{id}?append_to_response=keywords` detail and the
provider overview. Record the TMDB title/ID, relevant exact keyword names,
and an attributed short synopsis paraphrase. Do not infer a theme from cast
identity, title alone, benchmark membership, popularity, or the shallow score.
Keyword absence is missing evidence, not contrary evidence. A direct keyword
can support theme presence, but centrality requires an explicit main-story
statement in the synopsis or independently reviewed source. Broad `lgbt`
alone does not establish gay male content. `gay theme` and BL may suggest
presence; inspect the synopsis before assigning suitability. Record
`insufficient` when metadata cannot justify the claim. Mark `contradicted`
only for affirmative exclusion evidence. A lesbian lead, heterosexual main
romance, or other focus can coexist with male same-sex material; those facts
alone do not contradict a gay-male theme. Unknown narrow-theme evidence can
still be excluded from a required-theme response without claiming the work
is incompatible. Bisexual men and male same-sex relationships are eligible
when the requested theme is otherwise supported.

| Field | Values | Decision rule |
|---|---|---|
| Theme evidence | `central`, `present`, `insufficient`, `contradicted` | `central` needs explicit main-story gay-male content; `present` needs a direct narrower keyword or synopsis statement; missing/broad metadata is insufficient; `contradicted` needs affirmative exclusion evidence beyond another focus |
| Gay-theme suitability | 0–3 | 3 = clearly central gay-male story; 2 = substantial gay-male ensemble or relationship; 1 = incidental/weak fit or uncertain narrow theme; 0 = unrelated, contradicted, or too little evidence to recommend |
| Looking seed fit | 0–3 | 3 = adult gay-male friendship/dating or closely related urban life; 2 = substantive gay-male relationship or queer adult friendship; 1 = only broad LGBTQ or generic adult/urban overlap; 0 = unrelated or unsupported |
| Reviewer flag | `clear`, `adjudicate` | Flag plausible alternative grades, centrality, or provider text whose meaning is unclear |

A result is provisionally suitable when its route-specific grade is at least
2 and its evidence is attributable. For R3 the requested narrow theme is a
hard gate: a semantic score, broad `lgbt`, or seed similarity cannot turn
insufficient evidence into a displayed gay-male claim. For seed-only R4,
there is no mandatory gay-male filter. Adult queer friendship or ensemble
life can be a meaningful grade-2 connection to Looking even when the lead
characters are women; that grade is not a gay-male claim. General TV
popularity is not enough. Annotations
are about candidate quality, not claims that the eventual product already
implements these rules.

For each route report (a) how many suitable candidates exist anywhere in its
selected 30, (b) observed precision at positions 1–10 of the *fixed current
order*, (c) precision at all 30, (d) central/present/insufficient/
contradicted counts, and (e) adjudication flags. Do not reorder by the labels
and call that a working ranker. If fewer than ten suitable candidates exist
in the selected 30, report the exact count instead of scoring the route as
ten successful suggestions. Report separate results for R3 and Looking R4;
Looking is evaluated against its own seed, not the generic 30-title list.

## 2. Shallow-score check — fixed before inspecting candidate labels

The preregistered preselection adds 4 once for a narrow whole phrase, 2
once for a broad whole phrase, and 1 once for a romance whole phrase in the
provider summary. A small synthetic check will verify `gay couple` scores
4, `LGBTQ romance` scores 3, `relationship` scores 1, and an unrelated
summary scores 0. Repeated terms do not add extra points; `gay` inside
`gayer` must not match. The check validates the implementation of the
selection rule, not its relevance or model quality. Keep the synthetic
strings and observed scores with the results.

The following standalone Python 3 check uses the same whole-phrase lists and
one-hit-per-group arithmetic as the frozen selection rule. It was run on
2026-09-30; every assertion passed. Synthetic text avoids dependence on
provider wording and confirms both false positives and repeated terms.

```python
import re

narrow = ['gay', 'homosexual', 'same-sex', "boys' love", 'two men',
          'two boys', 'male couple', 'men in love']
broad = ['queer', 'lgbt', 'lgbtq']
romance = ['romance', 'relationship', 'fall in love']

def any_phrase(text, phrases):
    text = text.casefold()
    return any(re.search(r'(?<!\w)' + re.escape(p) + r'(?!\w)', text)
               for p in phrases)

def score(text):
    return (4 * any_phrase(text, narrow)
            + 2 * any_phrase(text, broad)
            + any_phrase(text, romance))

cases = {'gay couple': 4, 'LGBTQ romance': 3, 'relationship': 1,
         'ordinary school day': 0, 'gay gay': 4, 'gayer couple': 0,
         'gay, LGBTQ romance': 7}
for text, expected in cases.items():
    assert score(text) == expected, text
```

## 3. R3 theme candidates — provisional agent labels

Every title links to its TMDB series record. Keyword names and synopsis
paraphrases below come from the 2026-09-30 read-only TMDB detail response.
The paraphrases are short evidence notes, not reproduced provider summaries.
`C` = central · `P` = present · `I` = insufficient · `X` = contradicted.
The suitability grade follows the rubric in §1. `*` flags reviewer
adjudication. Position is the **fixed shallow-score order**, not a rerank
using these labels.

| # | Series (TMDB ID) | Relevant exact keywords | Synopsis evidence, paraphrased | Theme | Grade |
|---:|---|---|---|:---:|---:|
| 1 | [Love By Chance](https://www.themoviedb.org/tv/81318) 81318 | lgbt; boys' love (bl) | A gay student finds comfort with another boy after bullying; affection drives the story. | C | 3 |
| 2 | [Smiley](https://www.themoviedb.org/tv/214609) 214609 | lgbt; gay theme; boys' love (bl) | Two men in Barcelona search for love amid missed connections. | C | 3 |
| 3 | [TharnType](https://www.themoviedb.org/tv/93975) 93975 | lgbt; gay romance; gay relationship; boys' love (bl) | A student sharing a room with a gay man questions his hostile assumptions. | C | 3 |
| 4 | [Sasaki and Miyano](https://www.themoviedb.org/tv/127549) 127549 | boys' love (bl); romance | The two named boys move from meeting to a complicated romance. | C | 3 |
| 5 | [2gether: The Series](https://www.themoviedb.org/tv/99631) 99631 | lgbt; gay romance; gay relationship; boys' love (bl) | A college student's pretend boyfriend relationship becomes real. | C | 3 |
| 6 | [Eyewitness](https://www.themoviedb.org/tv/67429) 67429 | lgbt; lgbt teen; gay theme; boys' love (bl) | Two boys hide their relationship while facing danger after a shooting. | C | 3 |
| 7 | [Elite](https://www.themoviedb.org/tv/76669) 76669 | lgbt; gay muslim; gay theme | The overview centers school class conflict and tragedy; gay-male material is keyword-only here. | P | 1* |
| 8 | [Heartstopper](https://www.themoviedb.org/tv/124834) 124834 | lgbt; gay theme; gay relationship; boys' love (bl) | Charlie and Nick's friendship develops into young love. | C | 3 |
| 9 | [Riverdale](https://www.themoviedb.org/tv/69050) 69050 | lgbt; gay relationship; lesbian relationship | The overview describes town mysteries around Archie and friends; the male relationship is not developed there. | P | 1* |
| 10 | [Sex Education](https://www.themoviedb.org/tv/81356) 81356 | lgbt; lgbt teen; gay theme | A school sex-therapy clinic is the main synopsis; gay-male story is only tagged. | P | 1* |
| 11 | [given](https://www.themoviedb.org/tv/88040) 88040 | lgbt; boys' love (bl); romance | A boy's singing changes another boy's music and relationship. | C | 3 |
| 12 | [Ginny & Georgia](https://www.themoviedb.org/tv/117581) 117581 | gay theme | A mother and her children seek a fresh start; the overview gives no gay-male plot. | P | 1* |
| 13 | [Yuri!!! on Ice](https://www.themoviedb.org/tv/68129) 68129 | boys' love (bl); romance | The overview focuses on three male skaters and competitive renewal, without an explicit relationship. | P | 1* |
| 14 | [Heated Rivalry](https://www.themoviedb.org/tv/301507) 301507 | lgbt; gay theme; boys' love (bl); gay liberation | Two male hockey rivals sustain a secret relationship over years. | C | 3 |
| 15 | [American Horror Story](https://www.themoviedb.org/tv/1413) 1413 | lgbt | The overview lists unrelated horror-anthology settings, with no narrow theme statement. | I | 0 |
| 16 | [Love, Victor](https://www.themoviedb.org/tv/97186) 97186 | lgbt; lgbt teen; gay theme | Victor's orientation and coming-out journey drive his school story. | C | 3 |
| 17 | [Chucky](https://www.themoviedb.org/tv/90462) 90462 | lgbt; lgbt teen; gay youth | Murders and the doll's past lead the synopsis; the gay-youth tag supplies a side theme. | P | 1* |
| 18 | [Young Royals](https://www.themoviedb.org/tv/125910) 125910 | lgbt; lgbt teen; gay theme | A prince faces boarding-school life and romantic conflict; the male relationship is not explicit in this overview. | P | 2* |
| 19 | [KinnPorsche: The Series](https://www.themoviedb.org/tv/117067) 117067 | gay romance; gay relationship; boys' love (bl); gay love; lgbtq+ | A bodyguard's feelings for the male heir complicate danger and duty. | C | 3 |
| 20 | [Bridgerton](https://www.themoviedb.org/tv/91239) 91239 | lgbt; romance | The overview concerns Regency society and the Bridgerton family, without a narrow male theme. | I | 0 |
| 21 | [Merlí](https://www.themoviedb.org/tv/62914) 62914 | lgbt; gay romance; gay youth; gay theme | The overview centers a philosophy teacher and his students; a male romance is keyword-only. | P | 1* |
| 22 | [Hannibal](https://www.themoviedb.org/tv/40008) 40008 | lgbt; gothic romance | The synopsis centers an investigator and cannibalistic psychiatrist, without explicit gay-male content. | I | 0* |
| 23 | [Devilman Crybaby](https://www.themoviedb.org/tv/75208) 75208 | gay theme; bromance | A demon-boy and a male friend face a supernatural war; the theme is tagged, not made explicit in the synopsis. | P | 1* |
| 24 | [The Summer Hikaru Died](https://www.themoviedb.org/tv/278196) 278196 | queer; boys' love (bl) | One boy suspects something happened to his male best friend; relationship meaning is unclear here. | P | 1* |
| 25 | [DAHMER - Monster: The Jeffrey Dahmer Story](https://www.themoviedb.org/tv/113988) 113988 | lgbt | The overview concerns serial murders and institutional failure, without a suitable romance or identity story. | I | 0 |
| 26 | [XO, Kitty](https://www.themoviedb.org/tv/195670) 195670 | lgbt; gay theme; romance | Kitty seeks her boyfriend abroad; any gay-male material is outside the main synopsis. | P | 1* |
| 27 | [Good Omens](https://www.themoviedb.org/tv/71915) 71915 | lgbt | Two supernatural figures try to stop an apocalypse; no explicit narrow theme in the overview. | I | 0* |
| 28 | [Control Z](https://www.themoviedb.org/tv/102903) 102903 | lgbt | A student investigates a hacker exposing school secrets; narrow theme unestablished. | I | 0 |
| 29 | [Tiger King](https://www.themoviedb.org/tv/100698) 100698 | gay theme | The synopsis concerns big-cat breeding and a murder-for-hire case; tag alone does not make a suitable story. | P | 1* |
| 30 | [Semantic Error](https://www.themoviedb.org/tv/157208) 157208 | lgbt; gay romance; gay theme; boys' love (bl) | Two male university students' project conflict leads to attraction. | C | 3 |

**Observed fixed-order quality:** 13/30 are provisionally suitable at grade
at least 2. Positions 1–10 contain 7/10 suitable (precision@10 = 0.70);
this is below the proposed 0.80 release threshold. Twelve candidates have
central evidence, 12 have present evidence, six have insufficient evidence,
and zero are explicitly contradicted by the recorded metadata. Fourteen rows
are flagged for adjudication. Counting the thirteen suitable items anywhere
in 30 does not turn the observed top-ten order into a passing ranker.

## 4. Looking seed candidates — provisional agent labels

These are the fixed Looking R4 shallow-score positions. The theme column
still describes gay-male evidence; the grade is **fit to Looking's adult gay
men, friendship, dating, and urban-life premise**, not generic-list recall.
The same TMDB detail source and `*` adjudication marker as §3 apply.

| # | Series (TMDB ID) | Relevant exact keywords | Synopsis evidence, paraphrased | Theme | Seed fit |
|---:|---|---|---|:---:|---:|
| 1 | [Mid-Century Modern](https://www.themoviedb.org/tv/261363) 261363 | gay theme | Three older gay male friends form a Palm Springs household after a death. | C | 3 |
| 2 | [EastSiders](https://www.themoviedb.org/tv/67202) 67202 | lgbt; gay theme | A gay couple in Los Angeles confronts infidelity. | C | 3 |
| 3 | [Rain Dogs](https://www.themoviedb.org/tv/216795) 216795 | gay friend; gay theme | A gay man is part of a close adult friendship with a single mother and child. | P | 2* |
| 4 | [Uncoupled](https://www.themoviedb.org/tv/201380) 201380 | lgbt | A middle-aged gay man starts dating life again after his husband leaves. | C | 3 |
| 5 | [Cucumber](https://www.themoviedb.org/tv/61932) 61932 | lgbt; gay theme | The drama explores contemporary gay adult life after a disastrous date. | C | 3 |
| 6 | [Undateable](https://www.themoviedb.org/tv/51345) 51345 | friends; dating (no narrow keyword) | A recently out gay man is one friend in a straight man's roommate/dating sitcom. | P | 1* |
| 7 | [Demon Doctor](https://www.themoviedb.org/tv/207773) 207773 | lgbt | An LGBT-tagged demonologist and detective investigate supernatural mysteries; orientation is unspecified. | I | 1* |
| 8 | [Tales of the City](https://www.themoviedb.org/tv/87731) 87731 | lgbt; gay theme; transgender | A woman returns to San Francisco and her intergenerational queer chosen family. | P | 2* |
| 9 | [Banana](https://www.themoviedb.org/tv/61931) 61931 | lgbt; lesbian | LGBT anthology includes a male character's secret affair alongside lesbian stories. | P | 2* |
| 10 | [Sort Of](https://www.themoviedb.org/tv/133342) 133342 | lgbt | A gender-fluid adult balances queer spaces, family, and work; no gay-male story established. | I | 1* |
| 11 | [The L Word](https://www.themoviedb.org/tv/3475) 3475 | lesbian relationship; lgbt; lesbian | Los Angeles lesbian friends are the explicit central cast; gay-male presence is unknown. | I | 2 |
| 12 | [Master of None](https://www.themoviedb.org/tv/64254) 64254 | none narrow | A New York actor navigates adulthood and relationships; no gay-male premise is stated. | I | 1* |
| 13 | [Eli Stone](https://www.themoviedb.org/tv/1365) 1365 | none narrow | A corporate lawyer's visions and career choices drive the story. | I | 0 |
| 14 | [The Newsreader](https://www.themoviedb.org/tv/130842) 130842 | gay theme | A star newsreader and bisexual reporter work in a 1980s newsroom; narrow connection remains unclear. | P | 1* |
| 15 | [Crazy Ex-Girlfriend](https://www.themoviedb.org/tv/63161) 63161 | searching for love (generic) | A woman relocates to pursue love and happiness. | I | 0 |
| 16 | [Love, Victor](https://www.themoviedb.org/tv/97186) 97186 | lgbt; lgbt teen; gay theme | A male student's orientation and coming-out experience drive his story, though younger than Looking. | C | 2 |
| 17 | [Presidio Med](https://www.themoviedb.org/tv/24) 24 | none narrow | A medical drama centers a San Francisco hospital. | I | 0 |
| 18 | [Malcolm & Eddie](https://www.themoviedb.org/tv/71) 71 | none narrow | Two male roommates and coworkers are friends; no queer premise is stated. | I | 0 |
| 19 | [Tales of the City](https://www.themoviedb.org/tv/18745) 18745 | lgbt; gay theme | A woman enters a San Francisco apartment community with queer-themed metadata. | P | 2* |
| 20 | [Sex and the City](https://www.themoviedb.org/tv/105) 105 | love (generic) | Four women discuss careers, dating, and their New York social lives. | I | 1 |
| 21 | [The L Word: Generation Q](https://www.themoviedb.org/tv/89630) 89630 | lesbian relationship; lgbt; lesbian | A sequel follows lesbian leads and a broader queer Los Angeles ensemble; gay-male presence is unknown. | I | 2 |
| 22 | [El Chavo del Ocho](https://www.themoviedb.org/tv/47) 47 | none narrow | A child and neighbors encounter comic mishaps. | I | 0 |
| 23 | [That '70s Show](https://www.themoviedb.org/tv/52) 52 | teenage romance (generic) | Teen friends navigate suburban life, parents, and dating. | I | 0 |
| 24 | [Binoy Henyo](https://www.themoviedb.org/tv/58960) 58960 | love (generic) | A Filipino comedy-drama centers a young wonder child. | I | 0 |
| 25 | [Sexo Frágil](https://www.themoviedb.org/tv/92417) 92417 | none narrow | Four male friends focus on their relationships with women; narrow-theme evidence is absent. | I | 0 |
| 26 | [Franklin & Bash](https://www.themoviedb.org/tv/33301) 33301 | none narrow | Two lawyers chase clients and discuss women. | I | 0 |
| 27 | [Love Crossed](https://www.themoviedb.org/tv/125305) 125305 | romance (generic) | A woman enters a virtual world of idealized men after heartbreak; narrow-theme evidence is absent. | I | 0 |
| 28 | [The Full Monty](https://www.themoviedb.org/tv/196214) 196214 | gay interest | An older male friend group reunites for a caper; the tagged gay theme is not developed in the synopsis. | P | 1* |
| 29 | [Crescendo](https://www.themoviedb.org/tv/125312) 125312 | none narrow | Three friends establish a music company and pursue careers. | I | 0 |
| 30 | [And Just Like That…](https://www.themoviedb.org/tv/116450) 116450 | none narrow | Three women navigate later-life friendship and New York life. | I | 1 |

**Observed fixed-order quality after reviewer adjudication:** 11/30 are provisionally suitable at seed
fit at least 2. Positions 1–10 contain 7/10 suitable (precision@10 = 0.70).
The selected set contains at least ten plausible supported Looking-like
suggestions, though its current order remains below the precision target.
Five candidates have central gay-male evidence, seven have present evidence,
eighteen have insufficient gay-male evidence, and none has proven contrary
evidence. Ten rows remain flagged for adjudication. The geographic and
broad-LGBT overlap that makes some rows weakly related cannot be reported
as strong gay-male thematic evidence.

## 5. Adjudication and release meaning — current ranks do not pass

The star marks 14 R3 rows and ten Looking rows for further reviewer judgment. These
are the disagreements most likely to alter a decision; both possible
interpretations remain visible rather than being silently tuned to pass.

| Route / cases | Current call | Plausible alternative | Effect if changed |
|---|---|---|---|
| R3 `Elite`, `Sex Education`, `Riverdale` | Metadata-only grade 1: narrow tags but broad synopsis | Real-world grade 2 if an independent source shows a substantial gay-male arc | `Elite` or `Sex Education` would raise adjudicated precision@10 from 0.70 to 0.80; the frozen metadata-only order and grade remain recorded |
| R3 `Young Royals` | Present, grade 2 | Central, grade 3 with explicit main-story evidence | No precision@10 change; central-claim accuracy changes |
| R3 `Yuri!!! on Ice`, `The Summer Hikaru Died` | Present, grade 1 | Grade 2 if the male relationship is clearly thematic | Selected-30 sufficiency changes; neither is in top ten |
| R3 `Hannibal`, `Good Omens` | Insufficient, grade 0 | Present or suitable if independently documented queer relationship is part of the requested theme | Selected-30 count changes; no top-ten effect |
| Looking `Rain Dogs`, `Tales of the City` (both versions), `Banana` | Reviewer retains seed grade 2 from adult friendship/chosen-family/relationship evidence | A stricter notion of likeness could grade them 1 | Their seed fit does not assert an identical gay-male premise |
| Looking `Demon Doctor`, `The Newsreader`, `The Full Monty` | Grade 1 due sparse or different-focus TMDB metadata | Grade 2 only with independently verified seed-relevant material | Suitability may rise; do not silently feed external editorial material into runtime TMDB evidence |
| Looking `The L Word` and `Generation Q` | Gay-male theme unknown, seed fit 2 by reviewer adjudication | Adult queer friendship in Los Angeles is a meaningful seed connection | Both raise selected-set suitability from 9 to 11; the fixed top ten remains 7/10 |

These are **agent** labels over provider facts, not independently adjudicated
ground truth. The two fixed top tens each provisionally score 0.70, and the
Looking selected set has eleven plausible suitable items. Neither route's
**current order** meets the 0.80 precision@10 target. This unmodeled
baseline is not a gate against the M0b model experiment, whose purpose is to
test whether ordering improves. The experiment does not demonstrate the
required nDCG lift or standalone runtime behavior. The original 30-title and held-out 12-title
catalog recall measurements remain coverage/bias diagnostics in
[the revision report](smart-media-discovery-m0a-revision.md), not relevance
labels for this table.

**External relevance cross-check, separate from runtime TMDB evidence:**
[Netflix's own finale account](https://www.netflix.com/tudum/articles/sex-education-season-4-ending-explained)
describes Eric's gay identity, faith conflict, and substantial friendship
arc in *Sex Education*. That supports a provisional **real-world** grade 2
for an ensemble story. The fixed metadata-only row 10 remains grade 1; the
external article was neither a retrieval input nor a runtime reason. Likewise,
[ABC's account of *The Newsreader*](https://www.abc.net.au/news/2023-10-14/lgbtqia-journalists-on-challenges-they-ve-faced-in-the-media/102830668)
describes lead Dale's sexuality and threatened public outing. His bisexuality
does not negate queer thematic similarity. The Looking row remains seed
grade 1 under the TMDB-only baseline pending further seed-fit adjudication.
