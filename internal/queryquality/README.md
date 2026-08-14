# Query quality baseline

Production uses the fixed `minimal-v1 × deepseek-v4-pro` expansion baseline at temperature `0`, followed by criterion-aware lexical matching, deterministic selection, and the existing synthesis/citation authority. Synthesis remains `deepseek-chat`; both clients use the existing `DEEPSEEK_API_KEY` reference.

The DEV limitations are intentional and accepted for this slice:

- The semantic evaluator is unavailable; lexical hits never count as semantic proof.
- Unsupported or no-evidence queries may still fill irrelevant candidates.
- Equal-score tie quality remains unresolved.
