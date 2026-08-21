# Query quality baseline

Production uses the fixed `minimal-v1 × deepseek-v4-flash` expansion baseline at temperature `0`, with three bounded attempts in parallel and at most 24 normalized positive keywords per attempt. Results are aggregated by stable attempt index and keyword support before criterion-aware lexical matching, deterministic selection, and `deepseek-v4-pro` synthesis with configured reasoning. Expansion uses explicit disabled thinking; synthesis uses the configured thinking contract. Both clients use the existing `DEEPSEEK_API_KEY` reference.

The DEV limitations are intentional and accepted for this slice:

- The semantic evaluator is unavailable; lexical hits never count as semantic proof.
- Unsupported or no-evidence queries may still fill irrelevant candidates.
- Equal-score tie quality remains unresolved.

Lexical matching uses deterministic title, allowlisted frontmatter values, and body fields. Keyword support is one vote per normalized role/kind/value concept per attempt; normalized surface forms are retained for auditable local evidence, while production receipts retain only their digests.

Expansion prompt identities are production-owned built-ins: `minimal-v1` for
Lifestyle and `domain-neutral-technical-v1` for heterogeneous technical and
documentation retrieval. A prompt template digest is the full SHA-256 of the
compact JSON pair `[system_template,user_template]`; prompt text is not a stage
config or receipt field.
