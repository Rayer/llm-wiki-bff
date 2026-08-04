#!/usr/bin/env python3
"""Offline integration gate for the exact Synto 0.7.0 release.

This deliberately uses the pinned Python environment and exact CLI surfaces,
not a provider or a live LLM. It proves migrate-olw, Synto state creation, the
documented offline pack-export INDEX step, config preservation, and two
deterministic non-empty production sequences. OLW_BASELINE_ROOT points at the exact OLW
0.8.5 source tree used by the manual-edit parity companion. The two real
pack-export INDEX files and the raw source fixture are written to paths
provided by the Go bridge environment.
"""
from __future__ import annotations

import subprocess
import sys
import os
import sqlite3
import tempfile
import json
from datetime import datetime
from pathlib import Path
from unittest.mock import patch

bundle_helper_test = sys.argv[1:] == ["--test-publish-bridge-bundle"]

if not bundle_helper_test:
    from click.testing import CliRunner

    import synto
    import synto.client_factory as client_factory
    from synto.cli import _load_config, cli
    from synto.pipeline.compile import _content_hash as compile_content_hash
    from synto.pipeline.ingest import _content_hash as ingest_content_hash
    from synto.pipeline.ingest import _ingest_prompt_version
    from synto.models import RawNoteRecord, WikiArticleRecord
    from synto.state import StateDB
    from synto.vault import parse_note, write_note


if not bundle_helper_test and synto.__version__ != "0.7.0":
    raise SystemExit(f"expected synto 0.7.0, got {synto.__version__}")


class FailIfCalledProvider:
    """Test-only client: health is local, all provider work is forbidden."""

    def __init__(self) -> None:
        self.generate_calls = 0
        self.embed_calls = 0

    def require_healthy(self) -> None:
        pass

    def generate(self, *args, **kwargs):
        self.generate_calls += 1
        raise AssertionError("LLM generation endpoint was called")

    def embed(self, *args, **kwargs):
        self.embed_calls += 1
        raise AssertionError("LLM embedding endpoint was called")

    def embed_batch(self, *args, **kwargs):
        self.embed_calls += 1
        raise AssertionError("LLM embedding endpoint was called")

    def close(self) -> None:
        pass


provider = FailIfCalledProvider()


def fake_client_for(_resolved, _cache):
    return provider


def seed_bridge_article(vault: Path) -> None:
    """Add one deterministic published article for the cross-language gate.

    The article is written through exact-release StateDB/model seams only so
    the subsequent CLI pack export is still the real pinned exporter. No LLM
    or provider path is involved.
    """
    (vault / "raw").mkdir(exist_ok=True)
    raw_path = vault / "raw" / "source.md"
    raw_path.write_bytes(b"bridge source\n")
    write_note(
        vault / "wiki" / "Alpha.md",
        {"title": "Alpha", "id": "article-alpha", "sources": ["raw/source.md"]},
        "Bridge article body.",
    )
    config = _load_config(str(vault))
    _, raw_body = parse_note(raw_path)
    _, article_body = parse_note(vault / "wiki" / "Alpha.md")
    db = StateDB(vault / ".synto" / "state.db")
    try:
        db.upsert_raw(
            RawNoteRecord(
                path="raw/source.md",
                content_hash=ingest_content_hash(raw_body),
                status="compiled",
                language="en",
                quality="high",
                prompt_version=_ingest_prompt_version(config),
                ingested_at=datetime.now(),
                compiled_at=datetime.now(),
            )
        )
        db.upsert_concepts("raw/source.md", ["Alpha"])
        entity_id = db.entity_id_for_name("Alpha")
        if not entity_id:
            raise AssertionError("exact StateDB did not create Alpha entity")
        db.mark_concept_compile_state("Alpha", ["raw/source.md"], "compiled")
        db.upsert_article(
            WikiArticleRecord(
                path="wiki/Alpha.md",
                title="Alpha",
                sources=["raw/source.md"],
                content_hash=compile_content_hash(article_body),
                status="published",
                article_id="article-alpha",
                entity_id=entity_id,
            )
        )
        raw = db.get_raw("raw/source.md")
        compile_state = db.get_compile_state("Alpha", "raw/source.md")
        if raw is None or raw.content_hash != ingest_content_hash(raw_body):
            raise AssertionError("seeded raw row does not use Synto's exact body hash")
        if raw.prompt_version != _ingest_prompt_version(config):
            raise AssertionError("seeded raw row does not use the current ingest prompt fingerprint")
        if raw.status != "compiled":
            raise AssertionError(f"seeded raw row status is {raw.status!r}, want compiled")
        if compile_state is None or compile_state["status"] != "compiled":
            raise AssertionError(f"seeded compile state is not compiled: {compile_state}")
    finally:
        db.close()


run1_output = (os.environ.get("LWC195_EXACT_INDEX_RUN1_PATH") or "").strip()
run2_output = (os.environ.get("LWC195_EXACT_INDEX_RUN2_PATH") or "").strip()
raw_output = (os.environ.get("LWC195_RAW_SOURCE_PATH") or "").strip()
config_output = (os.environ.get("LWC197_MIGRATED_CONFIG_PATH") or "").strip()
if not bundle_helper_test and not all((run1_output, run2_output, raw_output, config_output)):
    raise SystemExit(
        "set LWC195_EXACT_INDEX_RUN1_PATH, LWC195_EXACT_INDEX_RUN2_PATH, "
        "LWC195_RAW_SOURCE_PATH, and LWC197_MIGRATED_CONFIG_PATH "
        "for the bridge artifact bundle"
    )


def clear_bridge_destinations(destinations: list[Path]) -> None:
    """Ensure a failed invocation cannot be mistaken for a prior successful bridge."""
    for path in destinations:
        path = path.expanduser()
        if path.exists() and not path.is_file():
            raise AssertionError(f"bridge destination is not a regular file: {path}")
        path.unlink(missing_ok=True)


def bridge_destinations() -> list[Path]:
    return [
        Path(run1_output).expanduser(),
        Path(run2_output).expanduser(),
        Path(raw_output).expanduser(),
        Path(config_output).expanduser(),
    ]


if not bundle_helper_test:
    clear_bridge_destinations(bridge_destinations())

def publish_bridge_file(staged: Path, destination: Path) -> None:
    """Atomically publish on the destination filesystem, including bind mounts."""
    import uuid

    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.parent / f".{destination.name}.{uuid.uuid4().hex}.tmp"
    try:
        with temporary.open("wb") as output:
            output.write(staged.read_bytes())
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)


def publish_bridge_bundle(
    staged_outputs: Path,
    outputs: list[tuple[str, Path]],
    publisher=publish_bridge_file,
) -> None:
    """Publish the complete bridge or remove every destination on failure."""
    destinations = [destination.expanduser() for _, destination in outputs]
    clear_bridge_destinations(destinations)
    try:
        for staged_name, destination in outputs:
            publisher(staged_outputs / staged_name, destination)
    except BaseException:
        clear_bridge_destinations(destinations)
        raise


def test_publish_bridge_bundle_rolls_back_injected_failure() -> None:
    with tempfile.TemporaryDirectory(prefix="lwc195-bridge-helper-") as temp:
        root = Path(temp)
        staged = root / "staged"
        staged.mkdir()
        outputs = [
            ("one", root / "dest-one"),
            ("two", root / "dest-two"),
            ("three", root / "dest-three"),
            ("four", root / "dest-four"),
        ]
        for _, destination in outputs:
            destination.write_bytes(b"stale")
        for staged_name, _ in outputs:
            (staged / staged_name).write_bytes(staged_name.encode())

        calls = 0

        def fail_on_second(staged_path: Path, destination: Path) -> None:
            nonlocal calls
            calls += 1
            if calls == 2:
                raise RuntimeError("injected bridge publication failure")
            publish_bridge_file(staged_path, destination)

        try:
            publish_bridge_bundle(staged, outputs, publisher=fail_on_second)
        except RuntimeError as error:
            if str(error) != "injected bridge publication failure":
                raise
        else:
            raise AssertionError("injected bridge publication failure was not raised")
        if calls != 2:
            raise AssertionError(f"injected publisher calls={calls}, want 2")
        if any(destination.exists() for _, destination in outputs):
            raise AssertionError("bundle rollback left a bridge destination behind")


if bundle_helper_test:
    test_publish_bridge_bundle_rolls_back_injected_failure()
    print("BUNDLE_PUBLICATION_ROLLBACK=PASS")
    raise SystemExit(0)


baseline = os.environ.get("OLW_BASELINE_ROOT")
if not baseline:
    raise SystemExit("set OLW_BASELINE_ROOT to the exact OLW 0.8.5 source tree")


def export_agents_index(vault: Path) -> bytes:
    with tempfile.TemporaryDirectory(prefix="lwc195-synto-pack-") as pack_dir:
        exported = CliRunner().invoke(
            cli,
            [
                "pack",
                "export",
                "--vault",
                str(vault),
                "--target",
                "agents",
                "--out",
                pack_dir,
            ],
        )
        if exported.exit_code:
            raise AssertionError(exported.output) from exported.exception
        exported_index = Path(pack_dir) / "index" / "INDEX.json"
        if not exported_index.is_file():
            raise AssertionError("pack export did not create index/INDEX.json")
        data = exported_index.read_bytes()
        if not data:
            raise AssertionError("pack export created an empty INDEX.json")
        return data


def alpha_article(payload: dict) -> dict:
    articles = [
        article
        for article in payload.get("articles", [])
        if article.get("path") == "articles/Alpha.md"
    ]
    if len(articles) != 1:
        raise AssertionError(f"expected one articles/Alpha.md entry, got {articles}")
    article = articles[0]
    if not article.get("id"):
        raise AssertionError(f"Alpha article lacks stable identity: {article}")
    return article


def alpha_source_edge(payload: dict) -> tuple[dict, str]:
    matches = [
        (edge, concept.get("entity_id"))
        for edge in payload.get("source_concepts", [])
        if edge.get("source_path") == "raw/source.md"
        for concept in edge.get("concepts", [])
        if concept.get("name") == "Alpha" and concept.get("entity_id")
    ]
    if len(matches) != 1:
        raise AssertionError(f"expected one Alpha/raw/source.md source edge, got {matches}")
    edge, entity_id = matches[0]
    # Exact Synto parses a plain markdown note to the body without its terminal newline.
    expected_hash = ingest_content_hash("bridge source")
    if edge.get("content_hash") != expected_hash:
        raise AssertionError(
            f"source edge hash {edge.get('content_hash')} != independently known {expected_hash}"
        )
    return edge, entity_id

with tempfile.TemporaryDirectory(prefix="lwc195-synto-exact-") as temp:
    vault = Path(temp)
    (vault / "wiki").mkdir()
    (vault / "wiki.toml").write_text(
        '[models]\nfast = "offline"\nheavy = "offline"\n', encoding="utf-8"
    )
    # migrate-olw only requires a legacy state file; a real SQLite file keeps
    # this gate honest without importing the old runtime.
    (vault / ".olw").mkdir()
    with sqlite3.connect(vault / ".olw" / "state.db"):
        pass

    migrated = CliRunner().invoke(cli, ["migrate-olw", "--vault", str(vault)])
    if migrated.exit_code:
        raise AssertionError(migrated.output) from migrated.exception
    synto_config_before = (vault / "synto.toml").read_bytes()
    staged_outputs = Path(temp) / "bridge-output"
    staged_outputs.mkdir()
    (staged_outputs / "migrated-synto.toml").write_bytes(synto_config_before)

    # Seed before exact CLI run #1. Both runs therefore exercise the same
    # non-empty article/entity state through the real orchestrator and pack
    # exporter; no INDEX is handcrafted here.
    seed_bridge_article(vault)

    # This is the worker's exact production sequence: the full orchestrator
    # run, followed by the documented offline pack export that emits the
    # authoritative schema-backed INDEX. No INDEX is handcrafted here.
    # Patch only Synto's exact-release client construction in this test process.
    # The real CLI _load_deps, ModelRouter, health gate, StateDB, orchestrator,
    # and pack exporter remain in the path. Production images never import this
    # test file, so this seam cannot alter worker runtime behavior.
    with patch.object(client_factory, "_build_client_for", side_effect=fake_client_for):
        first_run = CliRunner().invoke(cli, ["run", "--vault", str(vault), "--auto-approve"])
        if first_run.exit_code:
            raise AssertionError(first_run.output) from first_run.exception
        first = export_agents_index(vault)
        first_payload = json.loads(first)
        first_article = alpha_article(first_payload)
        first_edge, first_entity_id = alpha_source_edge(first_payload)
        (staged_outputs / "run1-INDEX.json").write_bytes(first)

        second_run = CliRunner().invoke(cli, ["run", "--vault", str(vault), "--auto-approve"])
        if second_run.exit_code:
            raise AssertionError(second_run.output) from second_run.exception
        second = export_agents_index(vault)
        second_payload = json.loads(second)
        second_article = alpha_article(second_payload)
        second_edge, second_entity_id = alpha_source_edge(second_payload)
        if first_article["id"] != second_article["id"]:
            raise AssertionError(f"article identity changed: {first_article} -> {second_article}")
        if first_entity_id != second_entity_id:
            raise AssertionError(f"engine entity identity changed: {first_entity_id} -> {second_entity_id}")
        if first_edge["content_hash"] != second_edge["content_hash"]:
            raise AssertionError("source edge hash changed across non-empty exports")
        (staged_outputs / "run2-INDEX.json").write_bytes(second)

    staged_raw = staged_outputs / "source.md"
    staged_raw.write_bytes(b"bridge source\n")

    if synto_config_before != (vault / "synto.toml").read_bytes():
        raise AssertionError("Synto config changed across the second run")
    if not (vault / ".synto" / "state.db").is_file():
        raise AssertionError("migrate-olw did not create .synto/state.db")
    if provider.generate_calls != 0:
        raise AssertionError(f"provider generation call count = {provider.generate_calls}, want 0")
    if provider.embed_calls != 0:
        raise AssertionError(f"provider embedding call count = {provider.embed_calls}, want 0")

    companion = Path(__file__).with_name("synto_manual_edit_smoke.py")
    result = subprocess.run([sys.executable, str(companion)], env=os.environ.copy(), check=False)
    if result.returncode:
        raise SystemExit(result.returncode)

    # Publish only after both exact CLI runs, both real pack exports, all identity/hash
    # assertions, and the manual-edit companion have passed. Each replacement is atomic;
    # destinations were cleared before the run so a failed invocation leaves no stale bridge.
    bridge_outputs = [
        ("run1-INDEX.json", Path(run1_output).expanduser()),
        ("run2-INDEX.json", Path(run2_output).expanduser()),
        ("source.md", Path(raw_output).expanduser()),
        ("migrated-synto.toml", Path(config_output).expanduser()),
    ]
    publish_bridge_bundle(staged_outputs, bridge_outputs)
    raw_path = Path(raw_output).expanduser()

    # Do not emit a PASS marker until every assertion, including the companion,
    # has succeeded.
    print("SYNTO_VERSION=0.7.0")
    print("MIGRATE_OLW=PASS")
    print(f"EXACT_MIGRATED_CONFIG_BYTES={len(synto_config_before)}")
    print(f"EXACT_MIGRATED_CONFIG_PATH={Path(config_output).expanduser()}")
    print("STATE_DB=PASS")
    print("EXACT_CLI_RUN_1=PASS")
    print("EXACT_CLI_PACK_EXPORT_1=PASS")
    print("EXACT_CLI_RUN_2=PASS")
    print("EXACT_CLI_PACK_EXPORT_2=PASS")
    print("EXACT_CLI_PACK_EXPORT_RUN1_NON_EMPTY=PASS")
    print("EXACT_CLI_PACK_EXPORT_RUN2_NON_EMPTY=PASS")
    print(f"EXACT_PACK_RUN1_BYTES={len(first)}")
    print(f"EXACT_PACK_RUN2_BYTES={len(second)}")
    print(f"EXACT_PACK_ARTICLE_ID={first_article['id']}")
    print(f"EXACT_PACK_ENGINE_ENTITY_ID={first_entity_id}")
    print("EXACT_PACK_RUN1_RUN2_ARTICLE_ENTITY_CONTINUITY=PASS")
    print("EXACT_PACK_SOURCE_EDGE_INDEPENDENT_HASH=PASS")
    print("EXACT_PACK_ARTICLE_PATH=articles/Alpha.md")
    print(f"EXACT_INDEX_RUN1_PATH={Path(run1_output).expanduser()}")
    print(f"EXACT_INDEX_RUN2_PATH={Path(run2_output).expanduser()}")
    print(f"RAW_SOURCE_PATH={raw_path}")
    print("INDEX_GENERATED_BY_PACK_EXPORT=PASS")
    print("TWO_OFFLINE_PRODUCTION_SEQUENCES=PASS")
    print("CONFIG_PRESERVED=PASS")
    print(f"PROVIDER_GENERATION_CALL_COUNT={provider.generate_calls}")
    print(f"PROVIDER_EMBED_CALL_COUNT={provider.embed_calls}")
    print("PROVIDER_ZERO_GENERATION_CALLS=PASS")
    print("PROVIDER_ZERO_EMBED_CALLS=PASS")
    print("MANUAL_EDIT_ZERO_PROVIDER_CALLS=PASS")
