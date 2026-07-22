#!/usr/bin/env python3
"""Offline exact-release Synto/OLW manual-edit parity smoke.

Run in the exact Synto 0.7.0 environment with OLW_BASELINE_ROOT pointing at
the exact OLW 0.8.5 source tree. This invokes the real CLI and never contacts
an LLM; the provider fails the test if called.
"""
from __future__ import annotations

import hashlib
import json
import os
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock

baseline = os.environ.get("OLW_BASELINE_ROOT")
if not baseline:
    raise SystemExit("set OLW_BASELINE_ROOT to the exact OLW 0.8.5 source tree")
sys.path.insert(0, str(Path(baseline) / "src"))

from click.testing import CliRunner
from obsidian_llm_wiki.models import RawNoteRecord, WikiArticleRecord
from obsidian_llm_wiki.state import StateDB as OLWStateDB
from synto.cli import cli
from synto.config import Config
from synto.pipeline.compile import compile_concepts
from synto.state import StateDB as SyntoStateDB


def digest(body: str) -> str:
    return hashlib.sha256(body.encode()).hexdigest()


class FailIfCalledRouter:
    def __init__(self) -> None:
        self.generate = MagicMock(side_effect=AssertionError("LLM provider was called"))

    def endpoint(self, _role: str):
        return SimpleNamespace(client=SimpleNamespace(generate=self.generate), model="offline-fake", ctx=8192, think=False, options={}, temperature=None)


with tempfile.TemporaryDirectory(prefix="lwc195-synto-manual-edit-") as temp:
    vault = Path(temp)
    (vault / "raw").mkdir()
    (vault / "wiki").mkdir()
    (vault / "raw" / "a.md").write_text("Source evidence.\n", encoding="utf-8")
    base_body = "AI generated baseline."
    human_body = "Human edited body that must survive migration."
    article = vault / "wiki" / "Alpha.md"
    article.write_text("---\ntitle: Alpha\nstatus: published\nsources:\n  - raw/a.md\n---\n\n" + human_body, encoding="utf-8")
    (vault / "wiki.toml").write_text('[models]\nfast = "fake"\nheavy = "fake"\n', encoding="utf-8")

    old = OLWStateDB(vault / ".olw" / "state.db")
    old.upsert_raw(RawNoteRecord(path="raw/a.md", content_hash="source-hash", status="ingested"))
    old.upsert_concepts("raw/a.md", ["Alpha"])
    old.upsert_article(WikiArticleRecord(path="wiki/Alpha.md", title="Alpha", sources=["raw/a.md"], content_hash=digest(base_body), is_draft=False))
    old.close()
    before = article.read_bytes()

    result = CliRunner().invoke(cli, ["migrate-olw", "--vault", str(vault)])
    if result.exit_code:
        raise AssertionError(result.output) from result.exception
    db = SyntoStateDB(vault / ".synto" / "state.db")
    migrated = db.get_article("wiki/Alpha.md")
    router = FailIfCalledRouter()
    drafts, failed, _ = compile_concepts(Config(vault=vault), router, db)
    state = db.get_compile_state("Alpha", "raw/a.md")
    observed = {
        "article_bytes_preserved": article.read_bytes() == before,
        "tracked_hash_preserved": migrated.content_hash == digest(base_body),
        "edited_body_differs": migrated.content_hash != digest(human_body),
        "drafts": [str(path.relative_to(vault)) for path in drafts],
        "failed": failed,
        "compile_state": state["status"] if state else None,
        "llm_calls": router.generate.call_count,
    }
    print(json.dumps(observed, indent=2))
    assert observed == {"article_bytes_preserved": True, "tracked_hash_preserved": True, "edited_body_differs": True, "drafts": [], "failed": [], "compile_state": "deferred_manual_edit", "llm_calls": 0}
    print("VERDICT=OLW_MANUAL_EDIT_BASELINE_PRESERVED_BY_SYNTO_MIGRATION")
