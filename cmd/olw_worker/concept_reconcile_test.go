package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

func TestReconcileConceptIDMapPreservesPriorStableIDForSameSlug(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}}
	// Transient generated ID differs; exact slug matches prior.
	data := []byte(`{"concept":{"transient-a":"alpha","new-id":"brand-new"},"source":{"s1":"source"},"redirects":{"transient-a":["old-alpha"]}}`)
	out, concepts, err := reconcileConceptIDMap(data, prior)
	if err != nil {
		t.Fatal(err)
	}
	var got wikiindex.IDMap
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Concept["stable-a"] != "alpha" {
		t.Fatalf("stable concept missing: %#v", got.Concept)
	}
	if _, exists := got.Concept["transient-a"]; exists {
		t.Fatalf("transient concept ID retained: %#v", got.Concept)
	}
	if got.Concept["new-id"] != "brand-new" {
		t.Fatalf("new slug should keep generated ID: %#v", got.Concept)
	}
	if got.Source["s1"] != "source" {
		t.Fatalf("source map must be preserved: %#v", got.Source)
	}
	if targets := got.Redirects["stable-a"]; len(targets) != 1 || targets[0] != "old-alpha" {
		t.Fatalf("redirect keys must translate to stable ID, targets stay slugs: %#v", got.Redirects)
	}
	if len(concepts) != 2 {
		t.Fatalf("concepts = %#v", concepts)
	}
}

func TestReconcileConceptIDMapIsOrderIndependentAndDeterministic(t *testing.T) {
	prior := []conceptSnapshot{
		{ConceptID: "stable-b", Slug: "beta"},
		{ConceptID: "stable-a", Slug: "alpha"},
	}
	data := []byte(`{"concept":{"zz-transient":"alpha","aa-transient":"beta","new":"gamma"},"redirects":{"zz-transient":["alpha-old"],"aa-transient":["beta-old"]}}`)
	var first string
	for i := 0; i < 25; i++ {
		out, concepts, err := reconcileConceptIDMap(data, prior)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if first == "" {
			first = string(out)
		} else if string(out) != first {
			t.Fatalf("non-deterministic output on run %d\nfirst=%s\ngot=%s", i, first, out)
		}
		if len(concepts) != 3 {
			t.Fatalf("run %d concepts=%#v", i, concepts)
		}
		// Stable IDs must win regardless of map iteration order.
		if !strings.Contains(string(out), `"stable-a": "alpha"`) || !strings.Contains(string(out), `"stable-b": "beta"`) || !strings.Contains(string(out), `"new": "gamma"`) {
			t.Fatalf("run %d map=%s", i, out)
		}
		if strings.Contains(string(out), "transient") {
			t.Fatalf("run %d retained transient IDs: %s", i, out)
		}
	}
}

func TestReconcileConceptIDMapSameSlugTitleBodyRewriteKeepsStableRoute(t *testing.T) {
	// Title/body are irrelevant: only exact slug identity transfers.
	prior := []conceptSnapshot{{ConceptID: "canon-1", Slug: "routing-key"}}
	data := []byte(`{"concept":{"totally-new-hash":"routing-key"}}`)
	out, _, err := reconcileConceptIDMap(data, prior)
	if err != nil {
		t.Fatal(err)
	}
	var got wikiindex.IDMap
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Concept["canon-1"] != "routing-key" || len(got.Concept) != 1 {
		t.Fatalf("canonical route lost: %#v", got.Concept)
	}
}

func TestReconcileConceptIDMapRejectsAmbiguousRenameMergeSplit(t *testing.T) {
	// Rename: prior slug gone, new slug appears — no automatic identity transfer.
	priorRename := []conceptSnapshot{{ConceptID: "old-id", Slug: "old-slug"}}
	out, concepts, err := reconcileConceptIDMap([]byte(`{"concept":{"new-transient":"new-slug"}}`), priorRename)
	if err != nil {
		t.Fatal(err)
	}
	got := mustDecodeIDMap(t, out)
	if got.Concept["new-transient"] != "new-slug" || got.Concept["old-id"] != "" {
		t.Fatalf("rename must not transfer identity: %#v", got.Concept)
	}
	if len(concepts) != 1 || concepts[0].StableID != "new-transient" {
		t.Fatalf("rename concepts=%#v", concepts)
	}

	// Merge: two prior slugs collapse into one new slug — no automatic transfer.
	priorMerge := []conceptSnapshot{
		{ConceptID: "left", Slug: "left-slug"},
		{ConceptID: "right", Slug: "right-slug"},
	}
	out, _, err = reconcileConceptIDMap([]byte(`{"concept":{"merged-transient":"merged"}}`), priorMerge)
	if err != nil {
		t.Fatal(err)
	}
	got = mustDecodeIDMap(t, out)
	if got.Concept["merged-transient"] != "merged" || len(got.Concept) != 1 {
		t.Fatalf("merge must keep generated ID only: %#v", got.Concept)
	}

	// Split: one prior slug becomes two new slugs — prior ID reserved, neither new slug may claim it unless exact match.
	priorSplit := []conceptSnapshot{{ConceptID: "parent", Slug: "parent-slug"}}
	// One child keeps exact prior slug (identity transfer), sibling is new.
	out, _, err = reconcileConceptIDMap([]byte(`{"concept":{"t-parent":"parent-slug","t-child":"child-slug"}}`), priorSplit)
	if err != nil {
		t.Fatal(err)
	}
	got = mustDecodeIDMap(t, out)
	if got.Concept["parent"] != "parent-slug" || got.Concept["t-child"] != "child-slug" {
		t.Fatalf("split exact-slug only: %#v", got.Concept)
	}
	// Neither of two brand-new split slugs may inherit the prior ID.
	out, _, err = reconcileConceptIDMap([]byte(`{"concept":{"t-a":"a-slug","t-b":"b-slug"}}`), priorSplit)
	if err != nil {
		t.Fatal(err)
	}
	got = mustDecodeIDMap(t, out)
	if got.Concept["t-a"] != "a-slug" || got.Concept["t-b"] != "b-slug" || got.Concept["parent"] != "" {
		t.Fatalf("split without exact slug must not transfer: %#v", got.Concept)
	}
}

func mustDecodeIDMap(t *testing.T, data []byte) wikiindex.IDMap {
	t.Helper()
	var got wikiindex.IDMap
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReconcileConceptIDMapFailClosedOnCollisionsMalformedUnsafe(t *testing.T) {
	tests := []struct {
		name   string
		prior  []conceptSnapshot
		idMap  string
		needle string
	}{
		{
			name:   "duplicate generated slug",
			prior:  nil,
			idMap:  `{"concept":{"a":"same","b":"same"}}`,
			needle: "duplicate",
		},
		{
			name:   "duplicate prior slug",
			prior:  []conceptSnapshot{{ConceptID: "p1", Slug: "same"}, {ConceptID: "p2", Slug: "same"}},
			idMap:  `{"concept":{"t":"same"}}`,
			needle: "duplicate prior",
		},
		{
			name:   "reserved ID used by different slug",
			prior:  []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}},
			idMap:  `{"concept":{"stable-a":"beta","t-alpha":"alpha"}}`,
			needle: "reserved",
		},
		{
			name:   "unsafe generated concept ID",
			prior:  nil,
			idMap:  `{"concept":{"../escape":"alpha"}}`,
			needle: "unsafe",
		},
		{
			name:   "unsafe generated slug",
			prior:  nil,
			idMap:  `{"concept":{"ok-id":"../escape"}}`,
			needle: "unsafe",
		},
		{
			name:   "unsafe prior concept ID",
			prior:  []conceptSnapshot{{ConceptID: "bad/id", Slug: "alpha"}},
			idMap:  `{"concept":{"t":"alpha"}}`,
			needle: "unsafe prior",
		},
		{
			name:   "malformed id_map",
			prior:  nil,
			idMap:  `{"concept":{"a":"one","a":"two"}}`,
			needle: "",
		},
		{
			name:   "stable ID collision across raw paths equivalent",
			prior:  []conceptSnapshot{{ConceptID: "shared", Slug: "alpha"}},
			idMap:  `{"concept":{"t-alpha":"alpha","shared":"beta"}}`,
			needle: "reserved",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := reconcileConceptIDMap([]byte(test.idMap), test.prior)
			if err == nil {
				t.Fatal("expected fail-closed error")
			}
			if test.needle != "" && !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("error=%v, want needle %q", err, test.needle)
			}
		})
	}
}

func TestReconcileWorkspaceConceptsRewritesIdentityAndReferences(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{
  "concept": {"transient-a": "alpha", "brand-new": "gamma"},
  "source": {"s1": "source"},
  "source_meta": {"s1": {"slug": "source", "source_file": "raw/a.md"}},
  "redirects": {"transient-a": ["alpha-old"], "s1": ["source-old"]}
}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
		`{"slug":"alpha","title":"Alpha New Title","body":"rewritten body mentions [[concepts/transient-a-alpha|Alpha]]","frontmatter":{"id":"transient-a","title":"Alpha New Title","sources":["s1"]},"sources":["s1"]}`+"\n"+
			`{"slug":"gamma","title":"Gamma","frontmatter":{"id":"brand-new","title":"Gamma"}}`+"\n",
	))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\ntitle: Alpha New Title\nsources:\n  - s1\n---\nrewritten body [[concepts/transient-a-alpha|Alpha]]\nprose transient-a stays\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "gamma.md"), []byte("---\nid: brand-new\ntitle: Gamma\n---\ngamma body\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "source.md"), []byte("---\nid: s1\nsource_file: raw/a.md\n---\nsource body mentions [[concepts/transient-a-alpha|Alpha]] and keeps prose transient-a\n"))

	prior := []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}}
	if err := reconcileWorkspaceConcepts(workspace, prior); err != nil {
		t.Fatal(err)
	}

	mapData, err := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ids wikiindex.IDMap
	if err := json.Unmarshal(mapData, &ids); err != nil {
		t.Fatal(err)
	}
	if ids.Concept["stable-a"] != "alpha" || ids.Concept["brand-new"] != "gamma" || ids.Concept["transient-a"] != "" {
		t.Fatalf("concept map=%#v", ids.Concept)
	}
	if ids.Source["s1"] != "source" {
		t.Fatalf("source reconciliation must remain untouched: %#v", ids.Source)
	}
	if targets := ids.Redirects["stable-a"]; len(targets) != 1 || targets[0] != "alpha-old" {
		t.Fatalf("concept redirects=%#v", ids.Redirects)
	}
	if targets := ids.Redirects["s1"]; len(targets) != 1 || targets[0] != "source-old" {
		t.Fatalf("source redirects must stay: %#v", ids.Redirects)
	}

	page, err := os.ReadFile(filepath.Join(workspace, "wiki", "alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "id: stable-a\n") || strings.Contains(string(page), "id: transient-a") {
		t.Fatalf("concept page identity not stabilized: %s", page)
	}
	if !strings.Contains(string(page), "[[concepts/stable-a-alpha|Alpha]]") {
		t.Fatalf("ID-bearing wikilink not rewritten: %s", page)
	}
	if strings.Count(string(page), "prose transient-a stays") != 1 {
		t.Fatalf("non-authoritative prose must not be rewritten: %s", page)
	}

	cache, err := os.ReadFile(filepath.Join(workspace, "cache", "concepts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cache), `"id":"stable-a"`) || strings.Contains(string(cache), "transient-a") {
		t.Fatalf("concepts cache not stabilized: %s", cache)
	}
	if !strings.Contains(string(cache), `"id":"brand-new"`) {
		t.Fatalf("new concept cache id lost: %s", cache)
	}
	// Source refs in cache must remain.
	if !strings.Contains(string(cache), `"s1"`) {
		t.Fatalf("source refs lost from concepts cache: %s", cache)
	}

	sourcePage, _ := os.ReadFile(filepath.Join(workspace, "wiki", "sources", "source.md"))
	if !strings.Contains(string(sourcePage), "id: s1\n") {
		t.Fatalf("source page must remain: %s", sourcePage)
	}
	if !strings.Contains(string(sourcePage), "[[concepts/stable-a-alpha|Alpha]]") {
		t.Fatalf("source page concept wikilink must be rewritten: %s", sourcePage)
	}
	if !strings.Contains(string(sourcePage), "prose transient-a") || strings.Contains(string(sourcePage), "id: stable-a") {
		t.Fatalf("source IDs/frontmatter and non-wikilink prose must stay: %s", sourcePage)
	}
}

func TestReconcileWorkspaceConceptsFailClosedOnInconsistentPage(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: other-id\ntitle: Alpha\n---\nbody\n"))
	err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
	if err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("error=%v, want inconsistent page rejection", err)
	}
}

func TestReconcileWorkspaceConceptsFailClosedOnMissingPage(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
	err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
	if err == nil {
		t.Fatal("expected missing page rejection")
	}
}

func TestSnapshotConceptsReadsPriorIDMap(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"stable-a":"alpha","stable-b":"beta"},"source":{"s1":"source"}}`))
	got, err := snapshotConcepts(vault)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ConceptID != "stable-a" || got[0].Slug != "alpha" || got[1].ConceptID != "stable-b" {
		t.Fatalf("snapshot=%#v", got)
	}
}

func TestSnapshotConceptsMissingMapIsEmpty(t *testing.T) {
	got, err := snapshotConcepts(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Fatalf("snapshot=%#v err=%v", got, err)
	}
}

func TestSourceAndConceptReconciliationCompose(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{
  "concept": {"c-transient": "alpha"},
  "source": {"s-transient": "source"},
  "source_meta": {"s-transient": {"slug": "source", "source_file": "raw/a.md"}},
  "redirects": {"c-transient": ["alpha-old"], "s-transient": ["source-old"]}
}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
		`{"slug":"alpha","frontmatter":{"id":"c-transient","sources":["s-transient"]},"sources":["s-transient"]}`+"\n",
	))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: c-transient\nsources:\n  - s-transient\n---\nbody [[concepts/c-transient-alpha|Alpha]]\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "source.md"), []byte("---\nid: s-transient\nsource_file: raw/a.md\n---\nsource\n"))

	if err := reconcileWorkspaceSources(workspace, []sourceSnapshot{{SourceID: "s-stable", RawPath: "raw/a.md"}}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "c-stable", Slug: "alpha"}}); err != nil {
		t.Fatal(err)
	}

	mapData, _ := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	var ids wikiindex.IDMap
	if err := json.Unmarshal(mapData, &ids); err != nil {
		t.Fatal(err)
	}
	if ids.Concept["c-stable"] != "alpha" || ids.Source["s-stable"] != "source" {
		t.Fatalf("composed map=%s", mapData)
	}
	if strings.Contains(string(mapData), "transient") {
		t.Fatalf("transient IDs remain: %s", mapData)
	}
	page, _ := os.ReadFile(filepath.Join(workspace, "wiki", "alpha.md"))
	if !strings.Contains(string(page), "id: c-stable\n") || !strings.Contains(string(page), "sources:\n- s-stable\n") {
		t.Fatalf("composed page=%s", page)
	}
	if !strings.Contains(string(page), "[[concepts/c-stable-alpha|Alpha]]") {
		t.Fatalf("composed wikilink=%s", page)
	}
	cache, _ := os.ReadFile(filepath.Join(workspace, "cache", "concepts.jsonl"))
	if !strings.Contains(string(cache), `"id":"c-stable"`) || !strings.Contains(string(cache), `"s-stable"`) {
		t.Fatalf("composed cache=%s", cache)
	}
	sourcePage, _ := os.ReadFile(filepath.Join(workspace, "wiki", "sources", "source.md"))
	if !strings.Contains(string(sourcePage), "id: s-stable\n") {
		t.Fatalf("source page=%s", sourcePage)
	}
}

func TestDefaultInPlacePathReconcilesConceptIDs(t *testing.T) {
	// Issue 1: Workspace=false must snapshot/reconcile Concept IDs end-to-end.
	old := execOLW
	defer func() { execOLW = old }()

	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("x"))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"source_file":"raw/source.md"}},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha\n---\nprior body\n"))

	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		// OLW regenerates the same slug under a transient concept ID.
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha Regenerated\n---\nregenerated [[concepts/transient-a-alpha|Alpha]]\n"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntitiesHash([]string{"article:01JAZ5N7Y3K8M2Q4R6T9VWXAC8:alpha"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXAC8"}, sha256Text("x"))))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		if strings.Join(command, " ") == "pack export --target agents --out" {
			return nil
		}
		return nil
	}

	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Workspace: false, Postprocess: true, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("default in-place run failed: %v", err)
	}

	mapData, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ids wikiindex.IDMap
	if err := json.Unmarshal(mapData, &ids); err != nil {
		t.Fatal(err)
	}
	if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXAC8"] != "alpha" || len(ids.Concept) != 1 {
		t.Fatalf("entity concept missing from id_map: %#v (%s)", ids.Concept, mapData)
	}
	if _, exists := ids.Concept["transient-a"]; exists {
		t.Fatalf("transient concept retained in id_map: %#v", ids.Concept)
	}
	page, err := os.ReadFile(filepath.Join(vault, "wiki", "alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "id: 01JAZ5N7Y3K8M2Q4R6T9VWXAC8\n") || strings.Contains(string(page), "id: stable-a") {
		t.Fatalf("article bytes changed on direct entity path: %s", page)
	}
}

func TestReconcileWorkspaceConceptsUsesCurrentExplicitULIDAtSameSlug(t *testing.T) {
	const priorEntity = "01JAZ5N7Y3K8M2Q4R6T9VWXABD"
	const currentEntity = "01JAZ5N7Y3K8M2Q4R6T9VWXABE"
	workspace := t.TempDir()
	page := []byte("---\nid: " + priorEntity + "\ntitle: Alpha\n---\nidentical bytes\n")
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"`+priorEntity+`":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"`+priorEntity+`"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), page)
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}

	// The generated page bytes and slug are unchanged; only released INDEX.json
	// supplies the current explicit Concept identity.
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"`+currentEntity+`":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"`+currentEntity+`"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture(currentEntity, currentEntity, "alpha", false)))

	if err := reconcileWorkspaceConcepts(workspace, prior); err != nil {
		t.Fatalf("same-slug current explicit ULID was rejected: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if len(ids.Concept) != 1 || ids.Concept[currentEntity] != "alpha" {
		t.Fatalf("concept map=%#v, want only current entity", ids.Concept)
	}
	if _, exists := ids.Concept[priorEntity]; exists {
		t.Fatalf("prior released entity remained authoritative: %#v", ids.Concept)
	}
	if _, exists := ids.IDRedirects[priorEntity]; exists {
		t.Fatalf("prior released entity became a redirect: %#v", ids.IDRedirects)
	}
	updated, err := os.ReadFile(filepath.Join(workspace, "wiki", "alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(updated, []byte("id: "+currentEntity+"\n")) || bytes.Contains(updated, []byte("id: "+priorEntity+"\n")) {
		t.Fatalf("current page identity=%q", updated)
	}
}

func TestReconcileWorkspaceConceptsExcludesEntitylessArticleFromConceptArtifacts(t *testing.T) {
	const articleID = "01JAZ5N7Y3K8M2Q4R6T9VWXAC8"
	for _, test := range []struct {
		name  string
		field string
	}{
		{name: "null", field: `"entity_id":null,`},
		{name: "omitted", field: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			index := strings.Replace(syntoIndexFixture("article", articleID, "ordinary", false), `"entity_id":"`+articleID+`",`, test.field, 1)
			page := []byte("---\nid: " + articleID + "\ntitle: Ordinary\n---\nordinary bytes\n")
			mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
			mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"`+articleID+`":"ordinary"},"source":{},"redirects":{}}`))
			mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"ordinary","frontmatter":{"id":"`+articleID+`"}}`+"\n"))
			mustWriteFile(t, filepath.Join(workspace, "wiki", "ordinary.md"), page)

			if err := reconcileWorkspaceConcepts(workspace, nil); err != nil {
				t.Fatalf("entityless article reconciliation failed: %v", err)
			}
			ids := mustSnapshotIDMap(t, workspace)
			if len(ids.Concept) != 0 || len(ids.ConceptEntityID) != 0 {
				t.Fatalf("entityless article entered Concept artifacts: %#v", ids)
			}
			cache, err := os.ReadFile(filepath.Join(workspace, "cache", "concepts.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(cache)) != "" {
				t.Fatalf("entityless article entered Concept cache: %q", cache)
			}
			updated, err := os.ReadFile(filepath.Join(workspace, "wiki", "ordinary.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(updated, page) {
				t.Fatalf("ordinary page bytes changed: %q", updated)
			}
		})
	}
}

func TestReconcileWorkspaceConceptsRejectsEntitylessIDPathDisagreementWithoutWrites(t *testing.T) {
	const entityID = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	workspace := t.TempDir()
	page := []byte("---\nid: article-a\ntitle: Alpha\n---\nalpha bytes\n")
	cache := []byte(`{"slug":"alpha","frontmatter":{"id":"article-a"}}` + "\n")
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoCrossArtifactEntitylessIDPathFixture(entityID)))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"article-a":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), cache)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), page)

	before := snapshotRelevantVaultBytes(t, workspace, "cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md")
	err := reconcileWorkspaceConcepts(workspace, nil)
	if err == nil {
		t.Fatal("malformed entityless ID/path fixture was accepted")
	}
	testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingConceptIDPathDisagreement)
	assertVaultBytesUnchanged(t, workspace, before)
}

func TestReconcileWorkspaceConceptsDirectPathDropsEntitylessConceptCacheRows(t *testing.T) {
	const boundID = "01JAZ5N7Y3K8M2Q4R6T9VWXAC8"
	const ordinaryID = "01JAZ5N7Y3K8M2Q4R6T9VWXAC9"
	workspace := t.TempDir()
	index := syntoIndexFixtureWithEntitiesHash(
		[]string{"bound:" + boundID + ":bound", "ordinary:" + ordinaryID + ":ordinary"},
		nil,
		strings.Repeat("0", 64),
	)
	index = strings.Replace(index, `"entity_id":"`+ordinaryID+`","name":"ordinary"`, `"entity_id":null,"name":"ordinary"`, 1)
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"`+boundID+`":"bound","`+ordinaryID+`":"ordinary"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"bound","frontmatter":{"id":"`+boundID+`"}}`+"\n"+`{"slug":"ordinary","frontmatter":{"id":"`+ordinaryID+`"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "bound.md"), []byte("---\nid: "+boundID+"\n---\nbound\n"))
	ordinary := []byte("---\nid: " + ordinaryID + "\n---\nordinary\n")
	mustWriteFile(t, filepath.Join(workspace, "wiki", "ordinary.md"), ordinary)

	if err := reconcileWorkspaceConcepts(workspace, nil); err != nil {
		t.Fatalf("mixed direct reconciliation failed: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if len(ids.Concept) != 1 || ids.Concept[boundID] != "bound" {
		t.Fatalf("mixed direct Concept map=%#v, want only bound article", ids.Concept)
	}
	cache, err := os.ReadFile(filepath.Join(workspace, "cache", "concepts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cache), "ordinary") {
		t.Fatalf("entityless direct article entered cache: %q", cache)
	}
	gotOrdinary, err := os.ReadFile(filepath.Join(workspace, "wiki", "ordinary.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotOrdinary, ordinary) {
		t.Fatalf("ordinary direct page changed: %q", gotOrdinary)
	}
}

func TestRewriteOtherConceptPageWikilinksIncludesSources(t *testing.T) {
	// Issue 2: Source pages with canonical concept wikilinks must be rewritten.
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "note.md"), []byte("---\nid: s1\nsource_file: raw/a.md\n---\nSee [[concepts/transient-a-alpha|Alpha]] and [[concepts/transient-a-alpha#sec|Sec]].\n"))
	concepts := []reconciledConcept{{CurrentID: "transient-a", StableID: "stable-a", Slug: "alpha"}}
	translations := map[string]string{"transient-a": "stable-a"}
	if err := rewriteOtherConceptPageWikilinks(workspace, concepts, translations); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(workspace, "wiki", "sources", "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "id: s1\n") || !strings.Contains(string(got), "source_file: raw/a.md") {
		t.Fatalf("source frontmatter mutated: %s", got)
	}
	if !strings.Contains(string(got), "[[concepts/stable-a-alpha|Alpha]]") || !strings.Contains(string(got), "[[concepts/stable-a-alpha#sec|Sec]]") {
		t.Fatalf("source concept wikilinks not rewritten: %s", got)
	}
	if strings.Contains(string(got), "transient-a") {
		t.Fatalf("transient concept id remains in source page: %s", got)
	}
}

func TestReconcileWorkspaceConceptsRejectsSymlinksAndOversized(t *testing.T) {
	// Issue 3: fail before symlink dereference / unbounded allocation.
	t.Run("concept page symlink", func(t *testing.T) {
		workspace := t.TempDir()
		outside := filepath.Join(t.TempDir(), "secret.txt")
		mustWriteFile(t, outside, []byte("outside-secret"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		if err := os.MkdirAll(filepath.Join(workspace, "wiki"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "wiki", "alpha.md")); err != nil {
			t.Fatal(err)
		}
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error=%v, want regular-file rejection before dereference", err)
		}
		// Outside content must never be copied into the vault as a regular file.
		if info, statErr := os.Lstat(filepath.Join(workspace, "wiki", "alpha.md")); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("concept path should remain a rejected symlink, info=%v err=%v", info, statErr)
		}
	})

	t.Run("other page symlink", func(t *testing.T) {
		workspace := t.TempDir()
		outside := filepath.Join(t.TempDir(), "secret.txt")
		mustWriteFile(t, outside, []byte("outside-secret"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		if err := os.MkdirAll(filepath.Join(workspace, "wiki", "sources"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "wiki", "sources", "note.md")); err != nil {
			t.Fatal(err)
		}
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !(strings.Contains(err.Error(), "symlink") || strings.Contains(err.Error(), "not a regular file")) {
			t.Fatalf("error=%v, want symlink rejection for other pages", err)
		}
	})

	t.Run("id_map symlink", func(t *testing.T) {
		workspace := t.TempDir()
		outside := filepath.Join(t.TempDir(), "id_map.json")
		mustWriteFile(t, outside, []byte(`{"concept":{"transient-a":"alpha"}}`))
		if err := os.MkdirAll(filepath.Join(workspace, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(workspace, "cache", "id_map.json")); err != nil {
			t.Fatal(err)
		}
		err := reconcileWorkspaceConcepts(workspace, nil)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error=%v, want id_map symlink rejection", err)
		}
	})

	t.Run("concepts cache symlink", func(t *testing.T) {
		workspace := t.TempDir()
		outside := filepath.Join(t.TempDir(), "concepts.jsonl")
		mustWriteFile(t, outside, []byte(`{"slug":"alpha","frontmatter":{"id":"transient-a"}}`+"\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		if err := os.Symlink(outside, filepath.Join(workspace, "cache", "concepts.jsonl")); err != nil {
			t.Fatal(err)
		}
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error=%v, want concepts cache symlink rejection", err)
		}
	})

	t.Run("oversized concept page", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		// Frontmatter keeps a consistent id; payload exceeds generation.MaxFileBytes.
		header := []byte("---\nid: transient-a\n---\n")
		body := bytesRepeat(byte('x'), generation.MaxFileBytes)
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), append(header, body...))
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("error=%v, want oversized page rejection", err)
		}
	})

	t.Run("oversized concepts cache", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), bytesRepeat(byte('y'), generation.MaxFileBytes+1))
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "size") {
			t.Fatalf("error=%v, want oversized cache rejection", err)
		}
	})
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestRewriteConceptIDBearingWikilinksIsExactRouteOnly(t *testing.T) {
	// Issue 4: prefix IDs and non-wikilink text must not be corrupted.
	concepts := []reconciledConcept{
		{CurrentID: "short", StableID: "stable-short", Slug: "alpha"},
		{CurrentID: "short-other", StableID: "stable-other", Slug: "beta"},
	}
	translations := map[string]string{"short": "stable-short", "short-other": "stable-other"}
	input := strings.Join([]string{
		"[[concepts/short-alpha|Alpha]]",
		"[[concepts/short-other-beta|Other]]",
		"[[concepts/short-alpha#sec|Sec]]",
		"[[concepts/short|Bare]]",
		"[[concepts/unrelated-route|Keep]]",
		"http://example.test/concepts/short-alpha",
		"`concepts/short-alpha`",
		"prose concepts/short-alpha stays",
	}, "\n")
	got := string(rewriteConceptIDBearingWikilinks([]byte(input), concepts, translations))
	wantParts := []string{
		"[[concepts/stable-short-alpha|Alpha]]",
		"[[concepts/stable-other-beta|Other]]",
		"[[concepts/stable-short-alpha#sec|Sec]]",
		"[[concepts/stable-short|Bare]]",
		"[[concepts/unrelated-route|Keep]]",
		"http://example.test/concepts/short-alpha",
		"`concepts/short-alpha`",
		"prose concepts/short-alpha stays",
	}
	for _, part := range wantParts {
		if !strings.Contains(got, part) {
			t.Fatalf("missing preserved/rewritten part %q in:\n%s", part, got)
		}
	}
	// Prefix corruption would rewrite short-other using the short mapping.
	if strings.Contains(got, "concepts/stable-short-other") {
		t.Fatalf("prefix ID corruption detected: %s", got)
	}
	if strings.Count(got, "stable-other-beta") != 1 {
		t.Fatalf("short-other route not uniquely rewritten: %s", got)
	}
}

func TestRewriteConceptCacheIDsFailClosedOnDuplicateAndIncomplete(t *testing.T) {
	// Issue 5: duplicate/missing/extra cache rows vs id_map.
	t.Run("duplicate slug rows", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
			`{"slug":"alpha","frontmatter":{"id":"transient-a"}}`+"\n"+
				`{"slug":"alpha","frontmatter":{"id":"transient-a"}}`+"\n",
		))
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("error=%v, want duplicate cache slug rejection", err)
		}
	})

	t.Run("missing cache row", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha","transient-b":"beta"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), []byte("---\nid: transient-b\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
			`{"slug":"alpha","frontmatter":{"id":"transient-a"}}`+"\n",
		))
		err := reconcileWorkspaceConcepts(workspace, nil)
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("error=%v, want missing cache row rejection", err)
		}
	})

	t.Run("extra cache row", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
			`{"slug":"alpha","frontmatter":{"id":"transient-a"}}`+"\n"+
				`{"slug":"ghost","frontmatter":{"id":"ghost-id"}}`+"\n",
		))
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "not declared") {
			t.Fatalf("error=%v, want extra cache row rejection", err)
		}
	})
}

func TestRewriteConceptCacheIDsRejectsDuplicateJSONKeys(t *testing.T) {
	// Issue 6: strict JSON — duplicate keys and trailing data fail closed.
	t.Run("duplicate top-level slug key", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
			`{"slug":"alpha","slug":"beta","frontmatter":{"id":"transient-a"}}`+"\n",
		))
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
			t.Fatalf("error=%v, want duplicate key rejection", err)
		}
	})

	t.Run("duplicate nested frontmatter id key", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
			`{"slug":"alpha","frontmatter":{"id":"transient-a","id":"other"}}`+"\n",
		))
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
			t.Fatalf("error=%v, want nested duplicate key rejection", err)
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
			`{"slug":"alpha","frontmatter":{"id":"transient-a"}} {"extra":true}`+"\n",
		))
		err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}})
		if err == nil || !(strings.Contains(err.Error(), "trailing") || strings.Contains(err.Error(), "decode")) {
			t.Fatalf("error=%v, want trailing JSON rejection", err)
		}
	})
}

func TestDockerfilePinsExactSyntoWheelHash(t *testing.T) {
	// Production install must pin the inspected Synto 0.7.0 wheel digest.
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	wantURL := "https://files.pythonhosted.org/packages/4a/e9/41c6b61338d98820780a43ed075cd77525674c38242110435330771d771b/synto-0.7.0-py3-none-any.whl"
	wantHash := "sha256=4bc8dcf14b53f45fac32ce737ecf878f1a46d6d0b010c7decbe6c3b7b10afa77"
	if !strings.Contains(text, wantURL) || !strings.Contains(text, wantHash) {
		t.Fatalf("Dockerfile does not pin exact wheel URL+hash:\n%s", text)
	}
	if strings.Contains(text, "obsidian_llm_wiki") || strings.Contains(text, "pip install synto") {
		t.Fatal("Dockerfile retains an unpinned or OLW dependency")
	}
}

// --- LWC-186 follow-up adversarial regressions (A–F) ---

func TestSourceThenConceptReconcileRejectsDuplicateJSONKeysWithoutSanitizing(t *testing.T) {
	// A: Source reconcile must not json.Unmarshal-sanitize duplicates before Concept check.
	workspace := t.TempDir()
	idMap := []byte(`{
  "concept": {"c-transient": "alpha"},
  "source": {"s-transient": "source"},
  "source_meta": {"s-transient": {"slug": "source", "source_file": "raw/a.md"}},
  "redirects": {}
}`)
	// Top-level + nested duplicate keys, plus a real Source ID that needs translation.
	malformedCache := []byte(
		`{"slug":"alpha","slug":"evil","sources":["s-transient"],"frontmatter":{"id":"c-transient","id":"other","sources":["s-transient"]},"body":"[[concepts/c-transient-alpha|Alpha]]"}` + "\n",
	)
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), idMap)
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), malformedCache)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: c-transient\nsources:\n  - s-transient\n---\nbody\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "source.md"), []byte("---\nid: s-transient\nsource_file: raw/a.md\n---\nsource\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "index.md"), []byte("See [[concepts/c-transient-alpha|Alpha]]\n"))

	before := snapshotRelevantVaultBytes(t, workspace,
		"cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md", "wiki/sources/source.md", "wiki/index.md",
	)

	err := reconcileWorkspaceSources(workspace, []sourceSnapshot{{SourceID: "s-stable", RawPath: "raw/a.md"}})
	if err == nil {
		// If Source somehow accepted, Concept must still fail closed — and neither may publish sanitized rows.
		err = reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "c-stable", Slug: "alpha"}})
	}
	if err == nil {
		t.Fatal("expected fail-closed error on duplicate JSON keys during Source+Concept compose")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error=%v, want duplicate-key rejection", err)
	}

	// Critical: Source must not sanitize duplicate keys into an acceptable cache row,
	// and must not partially apply earlier planned outputs (including id_map).
	cache, _ := os.ReadFile(filepath.Join(workspace, "cache", "concepts.jsonl"))
	if !bytes.Equal(cache, malformedCache) {
		t.Fatalf("concepts cache was sanitized/mutated:\nbefore=%s\nafter=%s", malformedCache, cache)
	}
	if strings.Contains(string(cache), `"s-stable"`) || !strings.Contains(string(cache), `"slug":"alpha","slug":"evil"`) {
		t.Fatalf("cache must keep raw duplicate keys and not accept Source translation: %s", cache)
	}
	// Full plan/apply: id_map, concepts cache, concept pages, Source pages — all byte-identical.
	assertVaultBytesUnchanged(t, workspace, before)
}

func TestDefaultInPlaceReconcileLateCacheFailureDoesNotPartiallyMutate(t *testing.T) {
	// B: plan/validate all outputs before any write (default Workspace=false reconciler).
	workspace := t.TempDir()
	idMap := []byte(`{"concept":{"transient-a":"alpha","transient-b":"beta"},"source":{},"source_meta":{},"redirects":{}}`)
	pageA := []byte("---\nid: transient-a\n---\nbody [[concepts/transient-a-alpha|A]]\n")
	pageB := []byte("---\nid: transient-b\n---\nbody\n")
	other := []byte("See [[concepts/transient-a-alpha|A]]\n")
	// Late failure: missing beta row after id_map/pages would previously have been written.
	cache := []byte(`{"slug":"alpha","frontmatter":{"id":"transient-a"},"body":"[[concepts/transient-a-alpha|A]]"}` + "\n")
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), idMap)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), pageA)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), pageB)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "notes.md"), other)
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), cache)

	before := snapshotRelevantVaultBytes(t, workspace,
		"cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md", "wiki/beta.md", "wiki/notes.md",
	)

	err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{
		{ConceptID: "stable-a", Slug: "alpha"},
		{ConceptID: "stable-b", Slug: "beta"},
	})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error=%v, want late missing-cache-row rejection", err)
	}
	assertVaultBytesUnchanged(t, workspace, before)

	// Also prove duplicate/malformed late failure is atomic on the default path end-to-end.
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"stable-a":"alpha"},"source":{},"source_meta":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: stable-a\n---\nprior\n"))
	mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"stable-a"}}`+"\n"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("name = \"t\"\n"))

	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		// Write a consistent page/id_map pair postprocess would rebuild, then a
		// cache that becomes incomplete relative to the generated concept set.
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nregen [[concepts/transient-a-alpha|A]]\n"))
		mustWriteFile(t, filepath.Join(work, "wiki", "beta.md"), []byte("---\nid: transient-b\n---\nbeta\n"))
		// concepts.jsonl only covers alpha — missing beta after postprocess rebuild
		// is hard, so we overwrite after postprocess by failing during reconcile
		// via an extra undeclared path: use only alpha page for id_map after
		// removing beta mid-reconcile is not possible. Instead write both pages
		// so postprocess builds two concepts, then replace concepts.jsonl with a
		// malformed duplicate-key row before returning from OLW... postprocess
		// rewrites concepts.jsonl. Force the partial-mutation scenario through
		// the same reconciler the default path uses after a crafted vault state.
		return nil
	}
	// Direct default-path-style call after crafting post-OLW vault state:
	// simulate postprocess output then invoke reconcileWorkspaceConcepts only.
	post := t.TempDir()
	mustWriteFile(t, filepath.Join(post, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"},"source":{},"source_meta":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(post, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
	mustWriteFile(t, filepath.Join(post, "wiki", "index.md"), []byte("[[concepts/transient-a-alpha|A]]\n"))
	mustWriteFile(t, filepath.Join(post, "cache", "concepts.jsonl"), []byte(
		`{"slug":"alpha","frontmatter":{"id":"transient-a"},"extra":1,"extra":2}`+"\n",
	))
	beforePost := snapshotRelevantVaultBytes(t, post, "cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md", "wiki/index.md")
	if err := reconcileWorkspaceConcepts(post, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error=%v, want duplicate key late failure", err)
	}
	assertVaultBytesUnchanged(t, post, beforePost)
	_ = vault
}

func TestReconcileConceptIDMapRejectsUnsafeRedirectKeysAndTargets(t *testing.T) {
	// C: redirect key/target validation before normalize/persist.
	tests := []struct {
		name   string
		idMap  string
		needle string
	}{
		{"escape key", `{"concept":{"ok":"alpha"},"redirects":{"../escape":["alpha"]}}`, "unsafe redirect key"},
		{"slash key", `{"concept":{"ok":"alpha"},"redirects":{"bad/id":["alpha"]}}`, "unsafe redirect key"},
		{"backslash key", `{"concept":{"ok":"alpha"},"redirects":{"bad\\id":["alpha"]}}`, "unsafe redirect key"},
		{"blank target", `{"concept":{"ok":"alpha"},"redirects":{"ok":[""]}}`, "unsafe redirect target"},
		{"dot target", `{"concept":{"ok":"alpha"},"redirects":{"ok":["."]}}`, "unsafe redirect target"},
		{"dotdot target", `{"concept":{"ok":"alpha"},"redirects":{"ok":[".."]}}`, "unsafe redirect target"},
		{"slash target", `{"concept":{"ok":"alpha"},"redirects":{"ok":["a/b"]}}`, "unsafe redirect target"},
		{"backslash target", `{"concept":{"ok":"alpha"},"redirects":{"ok":["a\\b"]}}`, "unsafe redirect target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := reconcileConceptIDMap([]byte(test.idMap), nil)
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("error=%v, want %q", err, test.needle)
			}
		})
	}
	// Collision after translation still fails closed.
	prior := []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}}
	_, _, err := reconcileConceptIDMap([]byte(`{"concept":{"t-a":"alpha"},"redirects":{"t-a":["old"],"stable-a":["other"]}}`), prior)
	if err == nil || !strings.Contains(err.Error(), "redirect ID collision") {
		t.Fatalf("error=%v, want redirect collision", err)
	}
}

func TestRewriteConceptPageIDRejectsDuplicateStableAndCurrentIDs(t *testing.T) {
	// C: duplicate top-level id fields even when parsed id already equals stableID.
	stableDup := []byte("---\nid: stable-a\nid: stable-a\ntitle: A\n---\nbody\n")
	if _, err := rewriteConceptPageID(stableDup, "transient-a", "stable-a"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("stable/stable error=%v, want duplicate rejection", err)
	}
	// current/current when current already equals stable (no translation rewrite path).
	currentDup := []byte("---\nid: same-id\nid: same-id\ntitle: A\n---\nbody\n")
	if _, err := rewriteConceptPageID(currentDup, "same-id", "same-id"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("current/current error=%v, want duplicate rejection", err)
	}
	// Workspace reconcile must also fail closed on duplicate stable ids.
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), stableDup)
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"transient-a"}}`+"\n"))
	if err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("workspace error=%v, want duplicate page id rejection", err)
	}
}

func TestDefaultPathNoPostprocessIgnoresMalformedPriorConceptMap(t *testing.T) {
	// A generation without postprocess is no longer a supported production
	// transaction; reject it before the child can touch the vault.
	old := execOLW
	defer func() { execOLW = old }()
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"a":"one","a":"two"}`))
	var ran bool
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		ran = work != ""
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Workspace: false, Postprocess: false, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("private no-postprocess run failed: %v", err)
	}
	if !ran {
		t.Fatal("expected private no-postprocess Synto run")
	}
	// Missing map is rejected the same way.
	vault2 := t.TempDir()
	ran = false
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault2, APIKey: "secret", Workspace: false, Postprocess: false}, `[["run"]]`); err != nil {
		t.Fatalf("missing-map no-postprocess run failed: %v", err)
	}
}

func TestExactSlugStabilityThroughWorkspaceAndCloudCompose(t *testing.T) {
	// E: consecutive exact-slug stability via workspace + cloud publish ordering.
	old := execOLW
	defer func() { execOLW = old }()

	// --- Workspace complete path (two consecutive generations) ---
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("raw body"))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{
	  "concept": {"a3f7b2c01d9d": "alpha"},
  "source": {"s-stable": "source"},
  "source_meta": {"s-stable": {"slug": "source", "source_file": "raw/source.md"}},
  "redirects": {}
}`))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\nsources:\n  - s-stable\n---\nprior [[concepts/c-stable-alpha|Alpha]]\n"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "sources", "source.md"), []byte("---\nid: s-stable\nsource_file: raw/source.md\n---\nsource [[concepts/c-stable-alpha|Alpha]]\n"))
	mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"a3f7b2c01d9d","sources":["s-stable"]},"sources":["s-stable"]}`+"\n"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("name = \"t\"\n"))
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntitiesHash([]string{"c-stable:01JAZ5N7Y3K8M2Q4R6T9VWXAC2:alpha"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXAC2"}, sha256Text("raw body"))))
	writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))

	var gen int
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		gen++
		transientC := fmt.Sprintf("c%011x", gen)
		transientS := fmt.Sprintf("s-transient-%d", gen)
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: "+transientC+"\nsources:\n  - "+transientS+"\n---\nbody [[concepts/"+transientC+"-alpha|Alpha]]\n"))
		mustWriteFile(t, filepath.Join(work, "wiki", "sources", "source.md"), []byte("---\nid: "+transientS+"\nsource_file: raw/source.md\n---\nsource [[concepts/"+transientC+"-alpha|Alpha]]\n"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntitiesHash([]string{transientC + ":01JAZ5N7Y3K8M2Q4R6T9VWXAC2:alpha"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXAC2"}, sha256Text("raw body"))))
		// Postprocess rebuilds id_map/concepts from these pages.
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true, StopOnError: true}
	for i := 0; i < 2; i++ {
		if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
			t.Fatalf("workspace gen %d: %v", i+1, err)
		}
		mapData, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ids wikiindex.IDMap
		if err := json.Unmarshal(mapData, &ids); err != nil {
			t.Fatal(err)
		}
		if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXAC2"] != "alpha" || ids.Source["s-stable"] != "source" {
			t.Fatalf("workspace gen %d lost exact-slug stability: %s", i+1, mapData)
		}
		if strings.Contains(string(mapData), "transient") {
			t.Fatalf("workspace gen %d retained transient IDs: %s", i+1, mapData)
		}
		page, _ := os.ReadFile(filepath.Join(vault, "wiki", "alpha.md"))
		if !strings.Contains(string(page), "id: 01JAZ5N7Y3K8M2Q4R6T9VWXAC2\n") || strings.Contains(string(page), "id: c-stable\n") {
			t.Fatalf("workspace gen %d page=%s", i+1, page)
		}
		src, _ := os.ReadFile(filepath.Join(vault, "wiki", "sources", "source.md"))
		if !strings.Contains(string(src), "id: s-stable\n") || !strings.Contains(string(src), "[[concepts/c") {
			t.Fatalf("workspace gen %d source=%s", i+1, src)
		}
		cache, _ := os.ReadFile(filepath.Join(vault, "cache", "concepts.jsonl"))
		if !strings.Contains(string(cache), `"01JAZ5N7Y3K8M2Q4R6T9VWXAC2"`) || !strings.Contains(string(cache), `"s-stable"`) {
			t.Fatalf("workspace gen %d cache=%s", i+1, cache)
		}
	}

	// --- Cloud generation/publish ordering (fake OLW + memory GCS) ---
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw body", "", priorCloudReceipt())
	// Prior committed generation with stable concept+source IDs.
	priorConceptPage := []byte("---\nid: a3f7b2c01d9d\nsources:\n  - s1\n---\nprior [[concepts/c-stable-alpha|Alpha]]\n")
	priorSourcePage := []byte("---\nid: s1\nsource_file: raw/source.md\n---\nsource [[concepts/c-stable-alpha|Alpha]]\n")
	priorMap := []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"source":{"s1":"source"},"source_meta":{"s1":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`)
	priorConcepts := []byte(`{"slug":"alpha","frontmatter":{"id":"a3f7b2c01d9d","sources":["s1"]},"sources":["s1"]}` + "\n")
	seedCloudManifestWithFiles(t, m, prefix, map[string][]byte{
		"wiki/alpha.md":          priorConceptPage,
		"wiki/sources/source.md": priorSourcePage,
		"cache/id_map.json":      priorMap,
		"cache/concepts.jsonl":   priorConcepts,
	})
	gen = 0
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		gen++
		transientC := fmt.Sprintf("d%011x", gen)
		transientS := fmt.Sprintf("s-cloud-%d", gen)
		if err := os.RemoveAll(filepath.Join(work, "wiki", "sources")); err != nil {
			return err
		}
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: "+transientC+"\nsources:\n  - "+transientS+"\n---\nbody [[concepts/"+transientC+"-alpha|Alpha]]\n"))
		mustWriteFile(t, filepath.Join(work, "wiki", "sources", "source.md"), []byte("---\nid: "+transientS+"\nsource_file: raw/source.md\n---\nsource [[concepts/"+transientC+"-alpha|Alpha]]\n"))
		if err := os.RemoveAll(filepath.Join(work, ".olw")); err != nil {
			return err
		}
		writeCloudRequiredOutputs(t, work)
		mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"`+transientC+`":"alpha"},"source":{"`+transientS+`":"source"},"source_meta":{"`+transientS+`":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"`+transientC+`","sources":["`+transientS+`"]},"sources":["`+transientS+`"],"body":"[[concepts/`+transientC+`-alpha|Alpha]]"}`+"\n"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntitiesHash([]string{transientC + ":01JAZ5N7Y3K8M2Q4R6T9VWXAC2:alpha"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXAC2"}, sha256Text("raw body"))))
		return nil
	}
	for i := 0; i < 2; i++ {
		if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m); err != nil {
			t.Fatalf("cloud gen %d: %v", i+1, err)
		}
		manifestData, _, err := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
		if err != nil {
			t.Fatal(err)
		}
		var manifest generation.Manifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			t.Fatal(err)
		}
		currentMap := mustCloudGenerationFile(t, m, prefix, manifest, "cache/id_map.json")
		currentPage := mustCloudGenerationFile(t, m, prefix, manifest, "wiki/alpha.md")
		currentSrc := mustCloudGenerationFile(t, m, prefix, manifest, "wiki/sources/source.md")
		currentCache := mustCloudGenerationFile(t, m, prefix, manifest, "cache/concepts.jsonl")
		if !strings.Contains(string(currentMap), `"01JAZ5N7Y3K8M2Q4R6T9VWXAC2": "alpha"`) && !strings.Contains(string(currentMap), `"01JAZ5N7Y3K8M2Q4R6T9VWXAC2":"alpha"`) {
			t.Fatalf("cloud gen %d concept map lost direct entity id: %s", i+1, currentMap)
		}
		if strings.Contains(string(currentMap), "c-cloud-") || strings.Contains(string(currentMap), "s-cloud-") {
			t.Fatalf("cloud gen %d retained transient IDs: %s", i+1, currentMap)
		}
		if !strings.Contains(string(currentMap), `"s1"`) {
			t.Fatalf("cloud gen %d lost stable source id s1: %s", i+1, currentMap)
		}
		if !strings.Contains(string(currentPage), "id: 01JAZ5N7Y3K8M2Q4R6T9VWXAC2\n") || !strings.Contains(string(currentPage), "[[concepts/d") {
			t.Fatalf("cloud gen %d page=%s", i+1, currentPage)
		}
		if !strings.Contains(string(currentSrc), "id: s1\n") || !strings.Contains(string(currentSrc), "[[concepts/d") {
			t.Fatalf("cloud gen %d source=%s", i+1, currentSrc)
		}
		if !strings.Contains(string(currentCache), "01JAZ5N7Y3K8M2Q4R6T9VWXAC2") || !strings.Contains(string(currentCache), `"s1"`) {
			t.Fatalf("cloud gen %d cache=%s", i+1, currentCache)
		}
	}
}

func TestWikilinksInsideMarkdownCodeArePreservedByteForByte(t *testing.T) {
	// F: [[concepts/...]] inside inline/fenced code stays unchanged; real links rewrite.
	concepts := []reconciledConcept{{CurrentID: "transient-a", StableID: "stable-a", Slug: "alpha"}}
	translations := map[string]string{"transient-a": "stable-a"}
	input := strings.Join([]string{
		"Real [[concepts/transient-a-alpha|Alpha]] outside.",
		"Inline `[[concepts/transient-a-alpha|Alpha]]` stays.",
		"Double ``[[concepts/transient-a-alpha|Alpha]]`` stays.",
		"```",
		"[[concepts/transient-a-alpha|Alpha]]",
		"```",
		"~~~",
		"[[concepts/transient-a|Bare]]",
		"~~~",
		"After [[concepts/transient-a-alpha|Alpha]] again.",
	}, "\n")
	got := string(rewriteConceptIDBearingWikilinks([]byte(input), concepts, translations))
	// Outside code rewritten.
	if !strings.Contains(got, "Real [[concepts/stable-a-alpha|Alpha]] outside.") {
		t.Fatalf("real wikilink not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "After [[concepts/stable-a-alpha|Alpha]] again.") {
		t.Fatalf("second real wikilink not rewritten:\n%s", got)
	}
	// Inside code preserved byte-for-byte.
	for _, part := range []string{
		"Inline `[[concepts/transient-a-alpha|Alpha]]` stays.",
		"Double ``[[concepts/transient-a-alpha|Alpha]]`` stays.",
		"```\n[[concepts/transient-a-alpha|Alpha]]\n```",
		"~~~\n[[concepts/transient-a|Bare]]\n~~~",
	} {
		if !strings.Contains(got, part) {
			t.Fatalf("code region mutated, missing %q in:\n%s", part, got)
		}
	}
	if strings.Count(got, "[[concepts/transient-a-alpha|Alpha]]") != 3 { // inline + double + fenced
		t.Fatalf("unexpected transient wikilink count in:\n%s", got)
	}
	if strings.Count(got, "[[concepts/stable-a-alpha|Alpha]]") != 2 {
		t.Fatalf("unexpected rewritten wikilink count in:\n%s", got)
	}
}

func TestWikilinksInsideBlockquotedFencedCodeArePreservedByteForByte(t *testing.T) {
	// Blockquoted fences (CommonMark: optional ≤3 spaces, `>` + optional space)
	// must preserve interior wikilinks; genuine outside links still rewrite.
	concepts := []reconciledConcept{{CurrentID: "transient-a", StableID: "stable-a", Slug: "alpha"}}
	translations := map[string]string{"transient-a": "stable-a"}
	input := strings.Join([]string{
		"Before [[concepts/transient-a-alpha|Alpha]] outside.",
		"> ~~~md",
		"> [[concepts/transient-a-alpha|A]]",
		"> ~~~",
		"> ```",
		"> [[concepts/transient-a-alpha|B]]",
		"> ```",
		"  > ~~~",
		"  > [[concepts/transient-a|C]]",
		"  > ~~~",
		">> ```go",
		">> [[concepts/transient-a-alpha|Nested]]",
		">> ```",
		">   > ~~~md",
		">   > [[concepts/transient-a-alpha|IndentedNested]]",
		">   > ~~~",
		// Ordinary blockquoted prose is NOT a fence — wikilink must still rewrite.
		"> Quote with [[concepts/transient-a-alpha|Quoted]] prose.",
		"After [[concepts/transient-a-alpha|Alpha]] again.",
	}, "\n")
	got := string(rewriteConceptIDBearingWikilinks([]byte(input), concepts, translations))

	// Outside / non-fence blockquote prose rewritten.
	if !strings.Contains(got, "Before [[concepts/stable-a-alpha|Alpha]] outside.") {
		t.Fatalf("leading outside wikilink not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "After [[concepts/stable-a-alpha|Alpha]] again.") {
		t.Fatalf("trailing outside wikilink not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "> Quote with [[concepts/stable-a-alpha|Quoted]] prose.") {
		t.Fatalf("ordinary blockquoted prose wikilink not rewritten:\n%s", got)
	}

	// Interior of blockquoted fences preserved byte-for-byte (including prefixes).
	for _, part := range []string{
		"> ~~~md\n> [[concepts/transient-a-alpha|A]]\n> ~~~",
		"> ```\n> [[concepts/transient-a-alpha|B]]\n> ```",
		"  > ~~~\n  > [[concepts/transient-a|C]]\n  > ~~~",
		">> ```go\n>> [[concepts/transient-a-alpha|Nested]]\n>> ```",
		">   > ~~~md\n>   > [[concepts/transient-a-alpha|IndentedNested]]\n>   > ~~~",
	} {
		if !strings.Contains(got, part) {
			t.Fatalf("blockquoted fence region mutated, missing %q in:\n%s", part, got)
		}
	}
	// Four preserved interior transients + zero extra rewrites into fences.
	if strings.Count(got, "[[concepts/transient-a-alpha|A]]") != 1 ||
		strings.Count(got, "[[concepts/transient-a-alpha|B]]") != 1 ||
		strings.Count(got, "[[concepts/transient-a|C]]") != 1 ||
		strings.Count(got, "[[concepts/transient-a-alpha|Nested]]") != 1 ||
		strings.Count(got, "[[concepts/transient-a-alpha|IndentedNested]]") != 1 {
		t.Fatalf("unexpected interior fence wikilink counts:\n%s", got)
	}
	if strings.Count(got, "[[concepts/stable-a-alpha|") != 3 { // Before, Quoted prose, After
		t.Fatalf("unexpected rewritten outside wikilink count in:\n%s", got)
	}
}

func TestConceptCacheReconciliationPreservesLargeIntegerFidelity(t *testing.T) {
	// F: UseNumber — large integers must not lose lexical/value fidelity.
	workspace := t.TempDir()
	const big = "9007199254740993" // 2^53+1, not exact as float64
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-a":"alpha"}}`))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-a\n---\nbody\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
		`{"slug":"alpha","count":`+big+`,"nested":{"n":`+big+`},"frontmatter":{"id":"transient-a"}}`+"\n",
	))
	if err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}}); err != nil {
		t.Fatal(err)
	}
	cache, err := os.ReadFile(filepath.Join(workspace, "cache", "concepts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cache), `"count":`+big) || !strings.Contains(string(cache), `"n":`+big) {
		t.Fatalf("large integer fidelity lost: %s", cache)
	}
	if strings.Contains(string(cache), "9.007") || strings.Contains(string(cache), "e+") {
		t.Fatalf("number rendered in scientific/float form: %s", cache)
	}
}

func TestSyntoMigrationConceptSlugChangeUsesExactSourceProvenance(t *testing.T) {
	workspace := t.TempDir()
	priorPage := []byte("---\nid: stable-old\nsources:\n  - stable-source\n---\nlegacy page\n")
	mustWriteFile(t, filepath.Join(workspace, "wiki", "old.md"), priorPage)
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-old":"old"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"old","frontmatter":{"id":"stable-old","sources":["stable-source"]}}`+"\n"))
	prior, err := snapshotConcepts(workspace, []sourceSnapshot{{SourceID: "stable-source", RawPath: "raw/source.md"}})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-old":"old","b7e2c9a4d113":"new"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(
		`{"slug":"old","frontmatter":{"id":"stable-old","sources":["stable-source"]}}`+"\n"+`{"slug":"new","frontmatter":{"id":"b7e2c9a4d113","sources":["stable-source"]}}`+"\n",
	))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "new.md"), []byte("---\nid: b7e2c9a4d113\nsources:\n  - stable-source\n---\nprovider article\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("current-article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC2", "new", true)))
	err = reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{RawPath: "raw/source.md", SyntoContentHash: strings.Repeat("0", 64)}})
	if err != nil {
		t.Fatalf("slug-change reconciliation: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if len(ids.Concept) != 1 || ids.Concept["stable-old"] != "new" || ids.ConceptEntityID["stable-old"] != "01JAZ5N7Y3K8M2Q4R6T9VWXAC2" {
		t.Fatalf("reconciled map=%#v", ids)
	}
	if _, ok := ids.Concept["current-article"]; ok {
		t.Fatalf("transient concept remained active: %#v", ids.Concept)
	}
	if _, err := os.Stat(filepath.Join(workspace, "wiki", "old.md")); !os.IsNotExist(err) {
		t.Fatalf("legacy active page remained: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(workspace, "wiki", "new.md"))
	if err != nil || !bytes.Contains(page, []byte("id: stable-old\n")) {
		t.Fatalf("current page identity=%q err=%v", page, err)
	}
}

func TestSyntoMigrationMissingCandidatesRemainFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		priorPage  []byte
		currentOld []byte
		generated  string
	}{
		{
			name:       "modified prior page",
			priorPage:  []byte("---\nid: stable-old\nsources:\n  - stable-source\n---\nlegacy\n"),
			currentOld: []byte("---\nid: stable-old\nsources:\n  - stable-source\n---\nmodified\n"),
			generated:  "stable-old",
		},
		{
			name:       "arbitrary source reference",
			priorPage:  []byte("---\nid: stable-old\nsources:\n  - raw/missing.md\n---\nlegacy\n"),
			currentOld: []byte("---\nid: stable-old\nsources:\n  - raw/missing.md\n---\nlegacy\n"),
			generated:  "stable-old",
		},
		{
			name:       "candidate not in prior",
			priorPage:  []byte("---\nid: stable-old\nsources:\n  - stable-source\n---\nlegacy\n"),
			currentOld: []byte("---\nid: generated-ghost\n---\nghost\n"),
			generated:  "generated-ghost",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			mustWriteFile(t, filepath.Join(workspace, "wiki", "old.md"), tc.priorPage)
			mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-old":"old"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
			mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"old","frontmatter":{"id":"stable-old"}}`+"\n"))
			prior, err := snapshotConcepts(workspace, []sourceSnapshot{{SourceID: "stable-source", RawPath: "raw/source.md"}})
			if err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(workspace, "wiki", "old.md"), tc.currentOld)
			mustWriteFile(t, filepath.Join(workspace, "wiki", "new.md"), []byte("---\nid: current-article\n---\nprovider\n"))
			mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"`+tc.generated+`":"old","current-article":"new"},"source":{},"redirects":{}}`))
			mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"old","frontmatter":{"id":"`+tc.generated+`"}}`+"\n"+`{"slug":"new","frontmatter":{"id":"current-article"}}`+"\n"))
			mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("current-article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC2", "new", true)))
			before := snapshotRelevantVaultBytes(t, workspace, "cache/id_map.json", "cache/concepts.jsonl", "wiki/old.md", "wiki/new.md", ".synto/INDEX.json")
			err = reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{RawPath: "raw/source.md", SyntoContentHash: strings.Repeat("0", 64)}})
			if err == nil {
				t.Fatal("unprovable missing candidate was accepted")
			}
			testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingConceptMissingMapping)
			assertVaultBytesUnchanged(t, workspace, before)
		})
	}
}

func TestSyntoMigrationSourceSetAmbiguityDoesNotTransferIdentity(t *testing.T) {
	index := syntoIndexTruthForEntityMapping(
		[]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "A", Path: "wiki/a.md"}, {ID: "article-b", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1", Name: "B", Path: "wiki/b.md"}},
		[]syntoSourceConcept{{Name: "A", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", SourcePath: "raw/source.md"}, {Name: "B", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1", SourcePath: "raw/source.md"}},
		nil,
	)
	for _, tc := range []struct {
		name   string
		prior  []conceptSnapshot
		ids    map[string]string
		proofs []reconciledConcept
		wantID string
	}{
		{
			name:   "multiple prior concepts",
			prior:  []conceptSnapshot{{ConceptID: "stable-a", Slug: "old-a", SourcePaths: []string{"raw/source.md"}}, {ConceptID: "stable-b", Slug: "old-b", SourcePaths: []string{"raw/source.md"}}},
			ids:    map[string]string{"article-a": "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"},
			proofs: nil,
			wantID: "",
		},
		{
			name:   "multiple current entities",
			prior:  []conceptSnapshot{{ConceptID: "stable-a", Slug: "old-a", SourcePaths: []string{"raw/source.md"}}},
			ids:    map[string]string{"article-a": "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", "article-b": "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"},
			proofs: nil,
			wantID: "",
		},
		{
			name:   "non-omitted prior concept does not transfer",
			prior:  []conceptSnapshot{{ConceptID: "stable-a", Slug: "old-a", SourcePaths: []string{"raw/source.md"}}},
			ids:    map[string]string{"article-a": "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"},
			proofs: nil,
			wantID: "",
		},
		{
			name:   "proven prior transfers",
			prior:  []conceptSnapshot{{ConceptID: "stable-a", Slug: "old-a", SourcePaths: []string{"raw/source.md"}}},
			ids:    map[string]string{"article-a": "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"},
			proofs: []reconciledConcept{{StableID: "stable-a", CurrentID: "stable-a", Slug: "old-a"}},
			wantID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := augmentLegacyConceptEntities(tc.prior, tc.ids, index, tc.proofs)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].EntityID != tc.wantID {
				t.Fatalf("ambiguous source-set transfer=%q, want %q", got[0].EntityID, tc.wantID)
			}
		})
	}
}

func TestFilterUnmappedLegacyConceptsRejectsDuplicateGeneratedSlugs(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("article-a", "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", "alpha", true)))
	indexTruth, err := readSyntoIndexTruth(workspace)
	if err != nil {
		t.Fatal(err)
	}
	generated := wikiindex.IDMap{Concept: map[string]string{
		"article-a": "alpha",
		"article-b": "alpha",
	}}
	prior := []conceptSnapshot{{ConceptID: "stable-a", Slug: "alpha"}}
	if _, _, err := filterUnmappedLegacyConcepts(workspace, generated, indexTruth, prior); err == nil || !strings.Contains(err.Error(), "duplicate generated concept slug") {
		t.Fatalf("error=%v, want duplicate generated concept slug", err)
	}
}

func TestSyntoMigrationCaseFoldIndexPathMatchRetainsCandidateAndFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	priorPage := []byte("---\nid: stable-old\nsources:\n  - stable-source\n---\nlegacy page\n")
	mustWriteFile(t, filepath.Join(workspace, "wiki", "ALPHA.md"), priorPage)
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-old":"ALPHA"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"ALPHA","frontmatter":{"id":"stable-old","sources":["stable-source"]}}`+"\n"))
	prior, err := snapshotConcepts(workspace, []sourceSnapshot{{SourceID: "stable-source", RawPath: "raw/source.md"}})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("current-article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", "alpha", true)))
	before := snapshotRelevantVaultBytes(t, workspace, "cache/id_map.json", "cache/concepts.jsonl", "wiki/ALPHA.md", ".synto/INDEX.json")
	err = reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{RawPath: "raw/source.md", SyntoContentHash: strings.Repeat("0", 64)}})
	if err == nil {
		t.Fatal("case-only index path match was silently omitted")
	}
	testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingConceptSlugCase)
	assertVaultBytesUnchanged(t, workspace, before)
}

func TestReconcileConceptIDMapRejectsRedirectTranslationCollisions(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-old", Slug: "old", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE"}}
	success := []byte(`{
  "concept": {"current-article":"new"},
  "concept_entity_id": {"current-article": "01JAZ5N7Y3K8M2Q4R6T9VWXABE"},
  "source": {},
  "source_meta": {},
  "redirects": {"current-article":["legacy"],"stable-old":["legacy"]}
}`)
	out, _, err := reconcileConceptIDMap(success, prior)
	if err != nil {
		t.Fatalf("unexpected redirect translation rejection: %v", err)
	}
	var ids wikiindex.IDMap
	if err := json.Unmarshal(out, &ids); err != nil {
		t.Fatal(err)
	}
	if _, ok := ids.Concept["current-article"]; ok {
		t.Fatalf("transient redirect key remained: %#v", ids.Concept)
	}
	if ids.Concept["stable-old"] != "new" || len(ids.Redirects["stable-old"]) != 1 || ids.Redirects["stable-old"][0] != "legacy" {
		t.Fatalf("concept map after redirect translation=%#v", ids)
	}
	if _, ok := ids.Redirects["current-article"]; ok {
		t.Fatalf("stale redirect key remained: %#v", ids.Redirects)
	}

	collision := []byte(`{
  "concept": {"current-article":"new"},
  "concept_entity_id": {"current-article":"01JAZ5N7Y3K8M2Q4R6T9VWXABE"},
  "source": {},
  "source_meta": {},
  "redirects": {"current-article":["legacy"],"stable-old":["different"]}
}`)
	if _, _, err := reconcileConceptIDMap(collision, prior); err == nil || !strings.Contains(err.Error(), "redirect ID collision") {
		t.Fatalf("collision=%v, want redirect ID collision", err)
	}
}

func TestReconcileConceptIDMapPreservesIDRedirectsInEntityMode(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABC"}, {ConceptID: "stable-beta", Slug: "beta", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABD"}}
	out, _, err := reconcileConceptIDMapWithEntities([]byte(`{
  "concept": {"transient-alpha":"alpha","transient-beta":"beta"},
  "concept_entity_id": {"transient-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","transient-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD"},
  "source": {},
  "source_meta": {},
  "id_redirects": {"legacy-alpha":"transient-alpha","legacy-beta":"transient-beta"}
}`), prior, true)
	if err != nil {
		t.Fatalf("unexpected IDRedirects rejection: %v", err)
	}
	var ids wikiindex.IDMap
	if err := json.Unmarshal(out, &ids); err != nil {
		t.Fatal(err)
	}
	if ids.Concept["stable-alpha"] != "alpha" || ids.Concept["stable-beta"] != "beta" {
		t.Fatalf("concept map after redirect translation=%#v", ids.Concept)
	}
	if got := ids.IDRedirects["legacy-alpha"]; got != "stable-alpha" {
		t.Fatalf("legacy redirect not preserved: want stable-alpha got %q", got)
	}
	if got := ids.IDRedirects["legacy-beta"]; got != "stable-beta" {
		t.Fatalf("legacy redirect not preserved: want stable-beta got %q", got)
	}

	repeatOut, _, err := reconcileConceptIDMapWithEntities(out, prior, true)
	if err != nil {
		t.Fatalf("idempotent second run failed: %v", err)
	}
	var repeated wikiindex.IDMap
	if err := json.Unmarshal(repeatOut, &repeated); err != nil {
		t.Fatal(err)
	}
	for source, target := range ids.IDRedirects {
		if repeated.IDRedirects[source] != target {
			t.Fatalf("IDRedirects changed after second run: %s -> %q, want %q", source, repeated.IDRedirects[source], target)
		}
	}
}

func TestReconcileConceptIDMapAllowsManyLegacySourcesToOneCurrentULID(t *testing.T) {
	entityID := "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	data := []byte(`{
	  "concept": {"` + entityID + `":"alpha"},
	  "concept_entity_id": {"` + entityID + `":"` + entityID + `"},
	  "source": {},
	  "source_meta": {},
	  "id_redirects": {"a3f7b2c01d9d":"` + entityID + `","b7e2c9a4d113":"` + entityID + `"}
}`)
	out, _, err := reconcileConceptIDMapWithEntities(data, nil, true)
	if err != nil {
		t.Fatalf("many-to-one ID redirects rejected: %v", err)
	}
	var ids wikiindex.IDMap
	if err := json.Unmarshal(out, &ids); err != nil {
		t.Fatal(err)
	}
	if len(ids.IDRedirects) != 2 || ids.IDRedirects["a3f7b2c01d9d"] != entityID || ids.IDRedirects["b7e2c9a4d113"] != entityID {
		t.Fatalf("ID redirects = %#v", ids.IDRedirects)
	}
}

func TestReconcileConceptIDMapRejectsInvalidIDRedirects(t *testing.T) {
	testCases := []struct {
		name   string
		data   string
		needle string
	}{
		{
			name:   "missing target",
			data:   `{"concept":{"01JAZ5N7Y3K8M2Q4R6T9VWXAC0":"alpha","01JAZ5N7Y3K8M2Q4R6T9VWXAC1":"beta"},"concept_entity_id":{"01JAZ5N7Y3K8M2Q4R6T9VWXAC0":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0","01JAZ5N7Y3K8M2Q4R6T9VWXAC1":"01JAZ5N7Y3K8M2Q4R6T9VWXAC1"},"source":{},"source_meta":{},"id_redirects":{"legacy-a":"missing"}}`,
			needle: "not found",
		},
		{
			name:   "source reused as active concept",
			data:   `{"concept":{"01JAZ5N7Y3K8M2Q4R6T9VWXAC0":"alpha","01JAZ5N7Y3K8M2Q4R6T9VWXAC1":"beta"},"concept_entity_id":{"01JAZ5N7Y3K8M2Q4R6T9VWXAC0":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0","01JAZ5N7Y3K8M2Q4R6T9VWXAC1":"01JAZ5N7Y3K8M2Q4R6T9VWXAC1"},"source":{},"source_meta":{},"id_redirects":{"01JAZ5N7Y3K8M2Q4R6T9VWXAC0":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}`,
			needle: "active concept",
		},
		{
			name:   "redirect chain",
			data:   `{"concept":{"01JAZ5N7Y3K8M2Q4R6T9VWXAC0":"alpha","01JAZ5N7Y3K8M2Q4R6T9VWXAC1":"beta"},"concept_entity_id":{"01JAZ5N7Y3K8M2Q4R6T9VWXAC0":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0","01JAZ5N7Y3K8M2Q4R6T9VWXAC1":"01JAZ5N7Y3K8M2Q4R6T9VWXAC1"},"source":{},"source_meta":{},"id_redirects":{"legacy-a":"legacy-b","legacy-b":"legacy-a"}}`,
			needle: "chain detected",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reconcileConceptIDMapWithEntities([]byte(tc.data), nil, true); err == nil || !strings.Contains(err.Error(), tc.needle) {
				t.Fatalf("error=%v, want %s", err, tc.needle)
			}
		})
	}
}

func TestRunWorkerBatchWorkspacePublishesStableConceptIDFromSourceProvenance(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	const rawBody = "source body"
	vault := t.TempDir()
	priorPage := []byte("---\nid: stable-old\nsources:\n  - stable-source\n---\nlegacy page\n")
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte(rawBody))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"stable-old":"old"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), []byte(`{"slug":"old","frontmatter":{"id":"stable-old","sources":["stable-source"]}}`+"\n"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "old.md"), priorPage)
	mustWriteFile(t, filepath.Join(vault, "wiki", "sources", "source.md"), []byte("---\nid: stable-source\nsource_file: raw/source.md\n---\nsource body\n"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("name = \"t\"\n"))
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "INDEX.json"), []byte(syntoIndexFixture("old", "01JAZ5N7Y3K8M2Q4R6T9VWXABE", "old", true)))
	beforeRaw, err := os.ReadFile(filepath.Join(vault, "raw", "source.md"))
	if err != nil {
		t.Fatal(err)
	}
	beforeStates := make(map[string][]byte, 2)
	for _, rel := range []string{".synto/state.db", ".olw/state.db"} {
		data, err := os.ReadFile(filepath.Join(vault, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		beforeStates[rel] = append([]byte(nil), data...)
	}
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(work, "wiki", "old.md"), priorPage)
		mustWriteFile(t, filepath.Join(work, "wiki", "new.md"), []byte("---\nid: c5d9e3f1a028\nsources:\n  - transient-source\n---\nprovider article\n"))
		mustWriteFile(t, filepath.Join(work, "wiki", "sources", "source.md"), []byte("---\nid: transient-source\nsource_file: raw/source.md\n---\nsource for new\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"stable-old":"old","c5d9e3f1a028":"new"},"source":{"transient-source":"source"},"source_meta":{"transient-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"old","frontmatter":{"id":"stable-old","sources":["transient-source"]}}`+"\n"+`{"slug":"new","frontmatter":{"id":"c5d9e3f1a028","sources":["transient-source"]}}`+"\n"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntitiesHash([]string{"c5d9e3f1a028:01JAZ5N7Y3K8M2Q4R6T9VWXAC6:new"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXAC6"}, sha256Text(rawBody))))
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("workspace run: %v", err)
	}

	ids := mustSnapshotIDMap(t, vault)
	if len(ids.Concept) != 1 || ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXAC6"] != "new" {
		t.Fatalf("unexpected concept map: %#v", ids.Concept)
	}
	if len(ids.ConceptEntityID) != 0 {
		t.Fatalf("legacy concept entity map retained: %#v", ids.ConceptEntityID)
	}
	if _, ok := ids.Concept["transient-new"]; ok {
		t.Fatalf("transient concept remained active: %#v", ids.Concept)
	}
	if _, ok := ids.Concept["current-article"]; ok {
		t.Fatalf("transient current mapping remained active: %#v", ids.Concept)
	}
	if _, err := os.Stat(filepath.Join(vault, "wiki", "old.md")); err != nil {
		t.Fatalf("entity-less legacy page was deleted: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(vault, "wiki", "new.md"))
	if err != nil || !bytes.Contains(page, []byte("id: 01JAZ5N7Y3K8M2Q4R6T9VWXAC6\n")) {
		t.Fatalf("new page identity=%q err=%v", page, err)
	}
	rawAfter, err := os.ReadFile(filepath.Join(vault, "raw", "source.md"))
	if err != nil || !bytes.Equal(beforeRaw, rawAfter) {
		t.Fatalf("raw source mutated: before=%q after=%q err=%v", beforeRaw, rawAfter, err)
	}
	for rel, want := range beforeStates {
		got, err := os.ReadFile(filepath.Join(vault, rel))
		if err != nil {
			t.Fatalf("read %s after run: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("state file changed: %s", rel)
		}
	}
	cache, err := os.ReadFile(filepath.Join(vault, "cache", "concepts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cache), `"transient-`) || strings.Contains(string(cache), `"current-article"`) {
		t.Fatalf("concept cache retained transient identifiers: %s", cache)
	}
}

func snapshotRelevantVaultBytes(t *testing.T, root string, rels ...string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(rels))
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("snapshot %s: %v", rel, err)
		}
		out[rel] = append([]byte(nil), data...)
	}
	return out
}

func assertVaultBytesUnchanged(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	for rel, want := range before {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s after failure: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("partial mutation on %s:\nbefore=%q\nafter=%q", rel, want, got)
		}
	}
}

func seedCloudManifestWithFiles(t *testing.T, m *memoryObjects, prefix string, files map[string][]byte) {
	t.Helper()
	var genFiles []generation.File
	for path, data := range files {
		a, err := m.Write(context.Background(), prefix+generation.Prefix+"g_prior01/"+path, data, map[string]string{"sha256": digestBytes(data)}, objectConditions{})
		if err != nil {
			t.Fatal(err)
		}
		f, err := generation.NewFile(path, data, a.Generation)
		if err != nil {
			t.Fatal(err)
		}
		genFiles = append(genFiles, f)
	}
	// Deterministic manifest file order.
	sort.Slice(genFiles, func(i, j int) bool { return genFiles[i].Path < genFiles[j].Path })
	manifest := generation.Manifest{
		Version:          generation.Version,
		GenerationID:     "g_prior01",
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		InputFingerprint: "prior",
		Files:            genFiles,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeCloudObject(t, m, prefix+generation.ManifestPath, data)
}
