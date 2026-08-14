# query_experiment

Run the experiment against one frozen Project snapshot:

```sh
go run ./cmd/query_experiment --snapshot ./frozen-project --cases ./cases.jsonl --runs 2 --output ./results.jsonl
```

The default `--service production` path is the actual baseline/control. It uses
the existing production query service and its existing output contract. The
three-host service is a trusted-local experiment only:

```sh
go run ./cmd/query_experiment --service three-host \
  --selection-limit 10 --exploration-slots 1 --seed -7 \
  --snapshot ./frozen-project --cases ./cases.jsonl
```

Three-host knobs are ignored by production. `--selection-limit` defaults to 10
and accepts 1..1000. `--exploration-slots` defaults to 1 and accepts 0 through
the selection limit. `--seed` accepts a signed 64-bit integer; when omitted,
the seed is derived reproducibly from the query. Selection is therefore causal
and exactly replayable for the same query, corpus, and knobs.

`--snapshot` must directly contain `cache/concepts.jsonl`:

```text
frozen-project/
├── cache/
│   └── concepts.jsonl
└── wiki/                 # optional; only needed if the cache requires page bodies
    └── coffee-shops.md
```

Cases are strict JSONL, one object per line:

```json
{"id":"coffee","query":"coffee shops","mode":"wiki"}
{"id":"full-coffee","query":"coffee shops","mode":"full"}
```

The no-key three-host path uses `deterministic-fallback`: one normalized raw
query as one preferred lexical criterion. This fallback is only a three-host
wiring smoke; it is not the baseline, ranking evidence, semantic evidence, or
quality evidence. A configured provider may make one structured expansion call;
provider failure falls back with a short trace reason. No project ontology or
semantic profile is loaded.

The three-host trace preserves the ordered `expansion`, `matching`, and
`selection` stages. It may include plan criteria, lexical field/term evidence,
eligibility reasons, selection reasons, exploration markers, and seed metadata,
but never concept bodies/snippets, prompts, credentials, or raw provider
responses. Semantic criteria are never treated as lexical proof; required or
excluded semantic criteria are explicitly unavailable without an evaluator.

Three-host returns selected identities only. It does not produce production
synthesis or citation resolution. The honest remaining limit is that this
trusted-local variant is an experiment, not a quality claim about production.

## Local fixture matrix

The trusted-local fixture runner is enabled only for `--service three-host` when
all three fixture files and `--artifacts-dir` are supplied. Production ignores
these flags, and the existing no-fixture three-host smoke remains unchanged.

```sh
go run ./cmd/query_experiment --service three-host \
  --snapshot ./frozen-project --cases ./cases.jsonl --runs 3 \
  --model-fixture ./models.json --models deepseek-v4-flash,deepseek-v4-pro,grok-4.6 \
  --profile-fixture ./profiles.json --profiles narrow \
  --prompt-fixture ./prompts.json --prompts concise,expanded \
  --selection-limit 10 --exploration-slots 1 --seed 7 \
  --artifacts-dir ./artifacts --summary ./summary.json --output ./results.jsonl
```

Model fixture entries are strict JSON objects with `id`, `provider`,
`base_url`, `model`, and `api_key`. Optional `temperature` and `reasoning`
are sent as OpenAI-compatible request parameters. The intended model IDs and
base URLs are `deepseek-v4-flash` and `deepseek-v4-pro` at
`https://api.deepseek.com`, and `grok-4.6` at `https://api.x.ai/v1`.
Keys are trusted-local input only and never appear in JSONL, receipts, errors,
logs, or summaries.

Profile fixture entries map directly to the narrow policy fields
`required_when_explicit`, `preferred_by_default`, and `goals_to_expand`.
Prompt entries contain `system_template` and `user_template`; the only
placeholders are `{{raw_query}}` and `{{criterion_policy}}`.
Selectors are comma-separated and preserve selector order. Empty selectors
select every entry in fixture order, producing the profile × prompt × model
Cartesian product.

Each attempt is written below
`<artifacts-dir>/<variant-id>/<case-id>/run-N/`: `request.json`,
`expansion.input.json`, `expansion.output.json`, `matching.input.json`,
`matching.output.json`, `selection.input.json`, `selection.output.json`, and
`final.json`. Inputs contain the exact prompts, full plan/policy, frozen corpus
identity and digest, candidate evidence, and effective selection seed. Outputs
contain the raw model response, parsed/fallback plan, all candidate evidence,
all selection decisions, and final identities. Bodies and snippets are never
written.

The summary reports attempt denominators for zero-result and under-five rates
(the threshold is recorded as `under_5`), recoverable and always-under-five
case rates, result-count min/max/mean/stddev, repeated-run exact result-set
matches, pairwise top-5/top-10 Jaccard, within-variant score drift, exact
selection replay for identical seed/input digest, fallback, latency and token
diagnostics. Optional case labels `known_positive_slugs`,
`forbidden_result_slugs`, and `tags` add labeled recall and forbidden-result
metrics; unlabeled cases do not contribute to those denominators.
