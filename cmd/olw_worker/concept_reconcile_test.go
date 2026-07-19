package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "source.md"), []byte("---\nid: s1\nsource_file: raw/a.md\n---\nsource body\n"))

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
