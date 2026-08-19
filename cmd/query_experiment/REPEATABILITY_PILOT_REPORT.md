# LWC-263 Repeatability Pilot Report (Sanitized)

## Evidence provenance and scope
- Source SHA: `3c36e8fb8c089b9050089c4c9579a122c582a011`
- Trusted-local run: `llm-wiki-bff-query-fixtures/runs/2026-08-18-repeatability-pilot-v1/`
- Frozen corpus digest: `e65809eb76e8e49e1860a0a2660fcfa7576a8e8c6b68be7c457cb0030554ffdc`
- Geometry: `1 × 2 × 3 × 6 × 3 = 108 attempts` (attempts).
- Selection settings: `limit=10`, `exploration=1`, `seed=7`.
- Execution: `2026-08-17T22:07:37.287505Z` → `2026-08-17T23:47:42.435625Z`, sequential and non-overlapping.
- Receipt integrity: `108` final result records, `864` attempt receipts, no missing/extra attempt receipt names.

## Deterministic metrics (machine-verified)
- Total attempt outcomes: `2` zero-result attempts, `2` under-5 result attempts, `0` forbidden hits.
- Transport/decoder anomaly totals: `1` fallback attempt and `1` provider/decode error attempt.
- Total attempts: `108`.

## Blind review totals and unblinding
- Total blind cells: `36`
- Ratings: `22 Poor`, `14 Acceptable`, `0 Good`
- First visible failure stage across cells: `10 Expansion`, `26 Selection`

Unblinding map used in this report:
- `Variant-A` = `extended × deepseek-v4-flash`
- `Variant-B` = `extended × deepseek-v4-pro`
- `Variant-C` = `extended × grok-4.6`
- `Variant-D` = `minimal × deepseek-v4-flash`
- `Variant-E` = `minimal × deepseek-v4-pro`
- `Variant-F` = `minimal × grok-4.6`

## Six-variant deterministic table
| Prompt × model | Recall@5 | Recall@10 | Forbidden | Exact pairwise result-set matches | Total duration median (min–max) | Host median | Combined non-host remainder median | Tokens | Failures |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| extended × Flash | 19/27 | 23/27 | 0/3 | 4/18 | 78,800.5 (24,154–151,100) ms | 150 ms | 78,652.5 ms | 172,812 | 0 |
| extended × Pro | 19/27 | 23/27 | 0/3 | 4/18 | 60,394 (41,211–118,380) ms | 152.5 ms | 60,223 ms | 145,458 | 0 |
| extended × Grok | 20/27 | 25/27 | 0/3 | 2/18 | 55,815 (34,126–69,452) ms | 55,706.5 ms | 134.5 ms | 81,752 | 0 |
| minimal × Flash | 19/27 | 23/27 | 0/3 | 2/18 | 34,483 (12,970–68,703) ms | 149.5 ms | 34,334 ms | 83,525 | 0 |
| minimal × Pro | 21/27 | 25/27 | 0/3 | 4/18 | 53,343 (26,274–78,503) ms | 149 ms | 53,190 ms | 90,192 (17 attempts with token-bearing usage) | 1 fallback/error |
| minimal × Grok | 18/27 | 21/27 | 0/3 | 7/18 | 43,240 (33,249–77,074) ms | 43,183.5 ms | 73.5 ms | 61,923 | 0 |

## Blind quality matrix after unblinding
Rows are case IDs, columns are prompt × model. Cells show rating and first-failure stage.

| Case | extended × Flash | extended × Pro | extended × Grok | minimal × Flash | minimal × Pro | minimal × Grok |
|---|---|---|---|---|---|---|
| exact-boven | Poor (Expansion) | Poor (Selection) | Poor (Expansion) | Poor (Expansion) | Poor (Selection) | Poor (Expansion) |
| explicit-exclusion | Acceptable (Selection) | Acceptable (Selection) | Acceptable (Selection) | Acceptable (Expansion) | Acceptable (Selection) | Poor (Expansion) |
| outdoor-water-play | Poor (Selection) | Poor (Expansion) | Acceptable (Selection) | Poor (Selection) | Acceptable (Selection) | Acceptable (Selection) |
| rainy-child-indoor | Poor (Selection) | Poor (Selection) | Poor (Selection) | Acceptable (Selection) | Poor (Selection) | Poor (Selection) |
| taipei-work-cafe | Acceptable (Selection) | Acceptable (Selection) | Acceptable (Selection) | Acceptable (Selection) | Acceptable (Expansion) | Poor (Expansion) |
| unsupported-skiing | Poor (Selection) | Poor (Selection) | Poor (Selection) | Poor (Selection) | Poor (Selection) | Poor (Expansion) |

## Representative failure analyses
1. `unsupported-skiing` (all variants): Selection repeatedly filled 10-item lists with non-skiing candidates while a truthful-empty behavior is expected when required semantic conditions are unsupported by selected evidence. This is a persistent precision/call-to-action failure in zero-result handling; selected evidence did not establish absolute corpus absence, only evidence insufficiency for the request.
2. `exact-boven` (all variants): Selection usually returned one known positive and many structurally plausible-but-unrelated places; exact entities are repeatedly diluted by weak required enforcement.
3. `outdoor-water-play`: extended × Grok had high recall (known-positive recall of 6/6 at Recall@5 and 6/6 at Recall@10) but unstable surrounding results; this is high-recall yet volatile stability.
4. `rainy-child-indoor`: minimal × Grok showed stable output shape across three runs (high repeatability signal) while still missing both known positives; this is explicitly a stable-but-wrong cell, separating repeatability from quality.
5. `minimal × Pro` had one transport fallback plus provider/decode error on the same attempt (taipei-work-cafe run 3), which changed candidate structure and should not be treated as normal semantic behavior.
6. Title-only judgments appear in several cells where non-title evidence was private and therefore not present in this sanitized artifact set. Those items were treated as bounded, non-grounded quality evidence and not as definitive semantic truth.

### Zero/non-zero contradictions
- `extended × Flash / unsupported-skiing`: runs 1–2 returned non-zero result sets, run 3 returned zero. The zero run uniquely introduced semantic excluded criteria, and Matcher’s semantic-unavailable path was fail-closed, amplifying the result to zero. This is not proof of corpus absence, only a contradiction of insufficient selected evidence under a strict policy.
- `extended × Pro / rainy-child-indoor`: run 1 returned zero, runs 2–3 returned non-zero. The zero run uniquely introduced a semantic excluded condition (`戶外`), and Matcher semantic-unavailable fail-closed amplified it to zero. This does not establish corpus non-existence, only insufficient evidence for requested conditions.
These two zero/non-zero contradictions are consistent with stage instability and selection policy effects.

## Timing interpretation
- Combined non-host remainder is used in this report because there is no dedicated Matcher/Selector stopwatch in the current fixtures.
- DeepSeek variants show host medians around `~149–152.5 ms` while remainders account for the majority of end-to-end latency.
- Grok variants show host medians around `~43.2–55.7 s` with very small combined remainders (`73.5–134.5 ms`).
- Plan-time/remainder correlation is observed only as a weak-to-moderate operational signal; it is not proof of causal attribution.

## Limitations and counterarguments
- This is a fixture-level repeatability pilot, not a full AnswerSynthesizer quality evaluation.
- Scope is Expansion → Matching → Selection and selected-identity output only; there is no synthesis, answer drafting, citation, or reasoning scoring in this harness.
- “Grok” in this pilot is the evaluated expansion fixture and not the human judge for final quality claims.
- Stable outputs can remain semantically wrong; repeatability and quality are separate axes and must not be conflated.

## Bounded recommendation
- Do not change any production/DEV default from this pilot alone.
- Preserve the separate future LWC-270 AnswerSynthesizer reasoning experiment for downstream correctness and phrasing decisions.
- If continuing, prioritize instrumentation for Match/Select stage timing and stricter required-condition handling before tuning prompts/models.
