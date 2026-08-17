# Query quality baseline

Production uses the fixed `minimal-v1 × deepseek-v4-flash` expansion baseline at temperature `0`, followed by criterion-aware lexical matching, deterministic selection, and `deepseek-v4-pro` synthesis with configured reasoning. Expansion uses explicit disabled thinking; synthesis uses the configured thinking contract. Both clients use the existing `DEEPSEEK_API_KEY` reference.

The DEV limitations are intentional and accepted for this slice:

- The semantic evaluator is unavailable; lexical hits never count as semantic proof.
- Unsupported or no-evidence queries may still fill irrelevant candidates.
- Equal-score tie quality remains unresolved.
