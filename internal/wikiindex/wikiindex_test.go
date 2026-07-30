package wikiindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/generation"
)

const testEntityULID = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
const testArticleULID = "01JAZ5N7Y3K8M2Q4R6T9VWXABD"

func TestDecodeSyntoIdentityPlanEnforcesReleasedEntityULID(t *testing.T) {
	for _, entityID := range []string{"entity-alpha", "01jaz5n7y3k8m2q4r6t9vw xabc", "01JAZ5N7Y3K8M2Q4R6T9VWXABCI", "8JAZ5N7Y3K8M2Q4R6T9VWXABC", "Z1JAZ5N7Y3K8M2Q4R6T9VWXABC"} {
		if _, err := DecodeSyntoIdentityPlan(syntoIdentityReleasedFixture(fmt.Sprintf(`%q`, entityID))); err == nil {
			t.Errorf("DecodeSyntoIdentityPlan accepted invalid entity ID %q", entityID)
		}
	}
	if plan, err := DecodeSyntoIdentityPlan(syntoIdentityReleasedFixture(fmt.Sprintf(`%q`, testEntityULID))); err != nil || plan.ByPath["wiki/ordinary.md"] != testEntityULID {
		t.Fatalf("valid released entity ID rejected: plan=%#v err=%v", plan, err)
	}
}

func TestDecodeSyntoIdentityPlanValidatesReleasedContainerContracts(t *testing.T) {
	base := string(syntoIdentityReleasedFixture("null"))
	tooManyTerms := `{"terms":[` + strings.TrimSuffix(strings.Repeat("null,", generation.MaxFiles+1), ",") + `]}`
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "pack field type", data: strings.Replace(base, `"id":"fixture"`, `"id":7`, 1)},
		{name: "missing pack field", data: strings.Replace(base, `"version":"0",`, "", 1)},
		{name: "invalid capability", data: strings.Replace(base, `"capabilities":["articles","concepts"]`, `"capabilities":["unknown"]`, 1)},
		{name: "missing stats field", data: strings.Replace(base, `,"failed_concept_count":0`, "", 1)},
		{name: "negative stats", data: strings.Replace(base, `"article_count":1`, `"article_count":-1`, 1)},
		{name: "unbounded logical array", data: strings.Replace(base, `"terms":[]`, tooManyTerms, 1)},
		{name: "duplicate source group", data: strings.Replace(base, `"source_concepts":[]`, `"source_concepts":[{"source_path":"raw/source.md","content_hash":"`+strings.Repeat("0", 64)+`","concepts":[]},{"source_path":"raw/source.md","content_hash":"`+strings.Repeat("1", 64)+`","concepts":[]}]`, 1)},
		{name: "duplicate source item", data: strings.Replace(base, `"source_concepts":[]`, `"source_concepts":[{"source_path":"raw/source.md","content_hash":"`+strings.Repeat("0", 64)+`","concepts":[{"name":"Alpha","entity_id":"`+testEntityULID+`"},{"name":"Alpha","entity_id":"`+testEntityULID+`"}]}]`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeSyntoIdentityPlan([]byte(tc.data)); err == nil {
				t.Fatal("malformed released INDEX unexpectedly accepted")
			}
		})
	}
}

func TestDecodeSyntoIdentityPlanMatchesWorkerGenericArrayContract(t *testing.T) {
	base := string(syntoIdentityReleasedFixture("null"))
	valid := strings.Replace(base, `"terms":[]`, `"terms":[null,true,7,"term",[1,2],{"nested":{"value":1}}]`, 1)
	valid = strings.Replace(valid, `"papers":[]`, `"papers":["paper",{"authors":["a","b"]}]`, 1)
	if _, err := DecodeSyntoIdentityPlan([]byte(valid)); err != nil {
		t.Fatalf("worker-compatible generic terms/papers rejected: %v", err)
	}

	duplicateNestedKey := strings.Replace(base, `"terms":[]`, `"terms":[{"nested":{"duplicate":1,"duplicate":2}}]`, 1)
	if _, err := DecodeSyntoIdentityPlan([]byte(duplicateNestedKey)); err == nil {
		t.Fatal("nested duplicate JSON key unexpectedly accepted")
	}
}

func TestRebuildWithSyntoIdentityRewritesCanonicalPageAndFailsBeforeWrites(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha\n---\nAlpha body")},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{},
	}
	if _, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
		ByPath: map[string]string{"wiki/alpha.md": testEntityULID}, ActiveEntities: map[string]bool{testEntityULID: true},
	}); err != nil {
		t.Fatal(err)
	}
	page := store.writes["wiki/alpha.md"]
	if !bytes.Contains(page, []byte("id: "+testEntityULID)) || bytes.Contains(page, []byte("a3f7b2c01d9d")) {
		t.Fatalf("canonical page=%q", page)
	}

	bad := &fakeStore{
		files: map[string][]MarkdownFile{"wiki/": {{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("---\nid: a3f7b2c01d9d\nid: " + testEntityULID + "\n---\nbody")}}, "wiki/sources/": {}},
		reads: map[string][]byte{},
	}
	if _, err := RebuildWithSyntoIdentity(context.Background(), bad, SyntoIdentityPlan{
		ByPath: map[string]string{"wiki/alpha.md": testEntityULID}, ActiveEntities: map[string]bool{testEntityULID: true},
	}); err == nil || len(bad.writes) != 0 {
		t.Fatalf("duplicate page ID result err=%v writes=%#v, want error and zero writes", err, bad.writes)
	}
}

func TestRebuildWithSyntoIdentityIgnoresPriorDifferentULIDAtSameSlug(t *testing.T) {
	const priorEntity = "01JAZ5N7Y3K8M2Q4R6T9VWXABD"
	const currentEntity = "01JAZ5N7Y3K8M2Q4R6T9VWXABE"
	oldJSON := []byte(`{"concept":{"` + priorEntity + `":"alpha"},"source":{},"redirects":{}}`)

	newStore := func(page []byte) *fakeStore {
		return &fakeStore{
			files: map[string][]MarkdownFile{
				"wiki/":         {{Slug: "alpha", Path: "wiki/alpha.md", Data: page}},
				"wiki/sources/": {},
			},
			reads: map[string][]byte{IDMapPath: oldJSON},
		}
	}

	t.Run("distinct entities with identical article bytes use current authority only", func(t *testing.T) {
		store := newStore([]byte("---\nid: " + priorEntity + "\ntitle: Alpha\n---\nidentical bytes\n"))
		next, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
			ByPath:         map[string]string{"wiki/alpha.md": currentEntity},
			ActiveEntities: map[string]bool{currentEntity: true},
		})
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if len(next.Concept) != 1 || next.Concept[currentEntity] != "alpha" {
			t.Fatalf("concept map = %#v, want only current entity", next.Concept)
		}
		if _, ok := next.Concept[priorEntity]; ok {
			t.Fatalf("prior entity remained authoritative: %#v", next.Concept)
		}
		if _, ok := next.IDRedirects[priorEntity]; ok {
			t.Fatalf("different prior ULID became a redirect: %#v", next.IDRedirects)
		}
	})

	t.Run("same entity with changed article content remains valid", func(t *testing.T) {
		store := newStore([]byte("---\nid: " + priorEntity + "\ntitle: Alpha\n---\nchanged content\n"))
		next, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
			ByPath:         map[string]string{"wiki/alpha.md": priorEntity},
			ActiveEntities: map[string]bool{priorEntity: true},
		})
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if len(next.Concept) != 1 || next.Concept[priorEntity] != "alpha" || len(next.IDRedirects) != 0 {
			t.Fatalf("same-entity rebuild = %#v redirects=%#v", next.Concept, next.IDRedirects)
		}
	})
}

func TestPlanSyntoIDRedirectsAllowsManyLegacySourcesToOneCurrentULID(t *testing.T) {
	const current = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	next := IDMap{Concept: map[string]string{current: "alpha"}}
	old := IDMap{
		Concept: map[string]string{
			"a3f7b2c01d9d": "alpha",
			"b7e2c9a4d113": "alpha",
		},
	}

	added, err := planSyntoIDRedirects(&next, old)
	if err != nil {
		t.Fatalf("many-to-one legacy migration rejected: %v", err)
	}
	if added != 2 || len(next.IDRedirects) != 2 {
		t.Fatalf("redirect count added=%d redirects=%#v, want two", added, next.IDRedirects)
	}
	for _, legacy := range []string{"a3f7b2c01d9d", "b7e2c9a4d113"} {
		if got := next.IDRedirects[legacy]; got != current {
			t.Fatalf("redirect %q = %q, want %q", legacy, got, current)
		}
	}
}

func TestRewriteSyntoConceptPageInsertsIDBeforeClosingDelimiter(t *testing.T) {
	got, err := RewriteSyntoConceptPage([]byte("---\ntitle: Alpha\n---\nBody"), testEntityULID)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("---\ntitle: Alpha\nid: " + testEntityULID + "\n---\nBody")
	if !bytes.Equal(got, want) {
		t.Fatalf("rewritten page = %q, want %q", got, want)
	}

	got, err = RewriteSyntoConceptPage([]byte("---\r\ntitle: Alpha\r\n---\r\nBody\r\n"), testEntityULID)
	if err != nil {
		t.Fatal(err)
	}
	want = []byte("---\r\ntitle: Alpha\r\nid: " + testEntityULID + "\r\n---\r\nBody\r\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("CRLF rewritten page = %q, want %q", got, want)
	}
}

func TestRewriteSyntoConceptPageValidatesCompleteYAMLBeforeMutation(t *testing.T) {
	valid := []byte("---\ntitle: Alpha\nlabels:\n  - one\n---\nBody")
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "duplicate title", data: []byte("---\ntitle: Alpha\ntitle: Beta\n---\nBody")},
		{name: "duplicate id", data: []byte("---\nid: a3f7b2c01d9d\nid: b7e2c9a4d113\n---\nBody")},
		{name: "complex key", data: []byte("---\n? [title, name]\n: Alpha\n---\nBody")},
		{name: "flow mapping", data: []byte("---\n{id: a3f7b2c01d9d, title: Alpha}\n---\nBody")},
		{name: "tagged id key", data: []byte("---\n!!str id: a3f7b2c01d9d\n---\nBody")},
		{name: "non-string id", data: []byte("---\nid: 7\n---\nBody")},
		{name: "multi-document", data: []byte("---\ntitle: Alpha\n--- # second document\ntitle: Beta\n---\nBody")},
		{name: "unterminated", data: []byte("---\ntitle: Alpha\nBody")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]byte(nil), tc.data...)
			if _, err := RewriteSyntoConceptPage(tc.data, testEntityULID); err == nil {
				t.Fatal("malformed frontmatter unexpectedly accepted")
			}
			if !bytes.Equal(tc.data, before) {
				t.Fatalf("input bytes changed on validation failure: %q", tc.data)
			}
		})
	}
	if _, err := RewriteSyntoConceptPage(valid, testEntityULID); err != nil {
		t.Fatalf("valid YAML rejected: %v", err)
	}
	bodyWithRule := []byte("---\ntitle: Alpha\n---\nParagraph\n\n---\n\nNext section\n")
	rewritten, err := RewriteSyntoConceptPage(bodyWithRule, testEntityULID)
	if err != nil {
		t.Fatalf("Markdown body horizontal rule rejected: %v", err)
	}
	wantBody := []byte("---\ntitle: Alpha\nid: " + testEntityULID + "\n---\nParagraph\n\n---\n\nNext section\n")
	if !bytes.Equal(rewritten, wantBody) {
		t.Fatalf("Markdown body changed: got %q want %q", rewritten, wantBody)
	}
	empty, err := RewriteSyntoConceptPage([]byte("---\n---\nBody\n"), testEntityULID)
	if err != nil {
		t.Fatalf("empty frontmatter rejected: %v", err)
	}
	wantEmpty := []byte("---\nid: " + testEntityULID + "\n---\nBody\n")
	if !bytes.Equal(empty, wantEmpty) {
		t.Fatalf("empty frontmatter rewrite = %q, want %q", empty, wantEmpty)
	}
	blockScalar := []byte("---\ntitle: Alpha\ndescription: |\n  Before\n  ---\n  After\n---\nBody\n")
	blockScalarRewritten, err := RewriteSyntoConceptPage(blockScalar, testEntityULID)
	if err != nil {
		t.Fatalf("indented block-scalar rule rejected: %v", err)
	}
	wantBlockScalar := []byte("---\ntitle: Alpha\ndescription: |\n  Before\n  ---\n  After\nid: " + testEntityULID + "\n---\nBody\n")
	if !bytes.Equal(blockScalarRewritten, wantBlockScalar) {
		t.Fatalf("block scalar changed: got %q want %q", blockScalarRewritten, wantBlockScalar)
	}
}

func TestRebuildWithSyntoIdentityUsesEntityIDsAndExcludesEntitylessPages(t *testing.T) {
	page := []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha\n---\nAlpha body")
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{Slug: "alpha", Path: "wiki/alpha.md", Data: page},
				{Slug: "draft", Path: "wiki/draft.md", Data: []byte("---\nid: old-draft-id\n---\nDraft body")},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{},
	}

	next, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
		ByPath:         map[string]string{"wiki/alpha.md": "01JAZ5N7Y3K8M2Q4R6T9VWXABC"},
		ActiveEntities: map[string]bool{"01JAZ5N7Y3K8M2Q4R6T9VWXABC": true},
	})
	if err != nil {
		t.Fatalf("RebuildWithSyntoIdentity() error = %v", err)
	}
	if got := next.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"]; got != "alpha" {
		t.Fatalf("concept entity map = %#v, want 01JAZ5N7Y3K8M2Q4R6T9VWXABC -> alpha", next.Concept)
	}
	if _, ok := next.Concept["a3f7b2c01d9d"]; ok {
		t.Fatalf("content-derived/frontmatter ID remained authoritative: %#v", next.Concept)
	}
	if len(next.ConceptEntityID) != 0 {
		t.Fatalf("legacy concept_entity_id map = %#v, want empty", next.ConceptEntityID)
	}
	if !bytes.Equal(store.files["wiki/"][0].Data, page) {
		t.Fatal("Synto-aware rebuild modified article bytes")
	}

	var entry struct {
		Slug        string                 `json:"slug"`
		Frontmatter map[string]interface{} `json:"frontmatter"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(store.writes[ConceptsJSONLPath]), &entry); err != nil {
		t.Fatalf("concept cache row is not valid JSON: %v", err)
	}
	if entry.Slug != "alpha" || entry.Frontmatter["id"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABC" {
		t.Fatalf("Synto concept cache row = %+v, want 01JAZ5N7Y3K8M2Q4R6T9VWXABE id", entry)
	}
	if strings.Contains(string(store.writes[ConceptsJSONLPath]), "draft") {
		t.Fatalf("entity-less page entered Synto concept cache: %s", store.writes[ConceptsJSONLPath])
	}

	store.files["wiki/"][0].Data = []byte("---\nid: b7e2c9a4d113\ntitle: Alpha\n---\nEdited body")
	second, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
		ByPath:         map[string]string{"wiki/alpha.md": "01JAZ5N7Y3K8M2Q4R6T9VWXABC"},
		ActiveEntities: map[string]bool{"01JAZ5N7Y3K8M2Q4R6T9VWXABC": true},
	})
	if err != nil {
		t.Fatalf("body-edit rebuild error = %v", err)
	}
	if got := second.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"]; got != "alpha" || len(second.Concept) != 1 {
		t.Fatalf("body edit changed entity identity = %#v", second.Concept)
	}
}

func TestDecodeSyntoIdentityPlanReleasedEntityShapes(t *testing.T) {
	tests := []struct {
		name       string
		entityJSON string
		wantEntity string
		wantErr    bool
	}{
		{name: "missing is entityless", wantEntity: ""},
		{name: "null is entityless", entityJSON: "null", wantEntity: ""},
		{name: "non-empty string", entityJSON: `"01JAZ5N7Y3K8M2Q4R6T9VWXABC"`, wantEntity: "01JAZ5N7Y3K8M2Q4R6T9VWXABC"},
		{name: "empty string", entityJSON: `""`, wantErr: true},
		{name: "number", entityJSON: "7", wantErr: true},
		{name: "object", entityJSON: `{}`, wantErr: true},
		{name: "array", entityJSON: `[]`, wantErr: true},
		{name: "boolean", entityJSON: "false", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := syntoIdentityReleasedFixture(tt.entityJSON)
			plan, err := DecodeSyntoIdentityPlan(data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("DecodeSyntoIdentityPlan() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got := plan.ByPath["wiki/ordinary.md"]; got != tt.wantEntity {
				t.Fatalf("ByPath[ordinary] = %q, want %q", got, tt.wantEntity)
			}
			if tt.name == "null is entityless" {
				page := []byte("ordinary page bytes")
				store := &fakeStore{files: map[string][]MarkdownFile{
					"wiki/":         {{Slug: "ordinary", Path: "wiki/ordinary.md", Data: page}},
					"wiki/sources/": {},
				}, reads: map[string][]byte{}}
				next, err := RebuildWithSyntoIdentity(context.Background(), store, plan)
				if err != nil {
					t.Fatalf("RebuildWithSyntoIdentity() error = %v", err)
				}
				if len(next.Concept) != 0 || !bytes.Equal(store.files["wiki/"][0].Data, page) || len(bytes.TrimSpace(store.writes[ConceptsJSONLPath])) != 0 {
					t.Fatalf("entityless page rebuild = concepts=%#v page=%q cache=%q", next.Concept, store.files["wiki/"][0].Data, store.writes[ConceptsJSONLPath])
				}
			}
		})
	}

	for name, data := range map[string][]byte{
		"trailing data":       append(syntoIdentityReleasedFixture("null"), []byte(" trailing")...),
		"duplicate entity_id": []byte(`{"schema_version":1,"pack":{},"articles":[{"id":"article-1","entity_id":null,"entity_id":null,"name":"Ordinary","path":"wiki/ordinary.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[],"synthesis":[],"stats":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSyntoIdentityPlan(data); err == nil {
				t.Fatal("DecodeSyntoIdentityPlan() unexpectedly accepted malformed JSON")
			}
		})
	}
}

func TestDecodeSyntoIdentityPlanDoesNotVetoExplicitEntityBySourceConceptName(t *testing.T) {
	entityID := testEntityULID
	entityConcept := "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"
	alternateConcept := "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"
	hash := strings.Repeat("0", 64)
	tests := []struct {
		name    string
		group   string
		wantLen int
	}{
		{
			name:    "single conflicting source concept",
			group:   `[{"source_path":"raw/source.md","content_hash":"` + hash + `","concepts":[{"name":"Ordinary","entity_id":"` + entityConcept + `"}]}]`,
			wantLen: 1,
		},
		{
			name:    "ambiguous conflicting source concept",
			group:   `[{"source_path":"raw/source.md","content_hash":"` + hash + `","concepts":[{"name":"Ordinary","entity_id":"` + entityConcept + `"},{"name":"Ordinary","entity_id":"` + alternateConcept + `"}]}]`,
			wantLen: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := DecodeSyntoIdentityPlan(syntoIdentityReleasedFixtureWithSourceConcepts(fmt.Sprintf(`%q`, entityID), tc.group))
			if err != nil {
				t.Fatalf("DecodeSyntoIdentityPlan() error = %v", err)
			}
			if got := plan.ByPath["wiki/ordinary.md"]; got != entityID {
				t.Fatalf("ByPath[wiki/ordinary.md] = %q, want %q", got, entityID)
			}
			if len(plan.ActiveEntities) != tc.wantLen {
				t.Fatalf("len(plan.ActiveEntities) = %d, want %d", len(plan.ActiveEntities), tc.wantLen)
			}
		})
	}
}

func TestDecodeSyntoIdentityPlanEntitylessRowsParticipateInValidation(t *testing.T) {
	entityBound := `{"id":"bound","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABE","name":"Bound","path":"wiki/bound.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`
	testCases := []struct {
		name   string
		rows   []string
		needle string
	}{
		{
			name: "duplicate ID with null and omitted entity",
			rows: []string{
				`{"id":"dup","entity_id":null,"name":"Duplicate","path":"wiki/first.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
				`{"id":"dup","name":"Duplicate","path":"wiki/second.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
			},
			needle: "duplicate Synto article ID",
		},
		{
			name: "duplicate slug with null and entityless rows",
			rows: []string{
				`{"id":"slug-a","entity_id":null,"name":"Alpha","path":"wiki/A.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
				`{"id":"slug-b","name":"Alpha","path":"wiki/a.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
				entityBound,
			},
			needle: "duplicate Synto article slug",
		},
		{
			name: "unsafe entityless path fails",
			rows: []string{
				`{"id":"safe","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABF","name":"Safe","path":"wiki/safe.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
				`{"id":"bad","name":"Bad","path":"wiki/../bad.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
			},
			needle: "unsafe Synto article path",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeSyntoIdentityPlan(syntoIdentityFixtureFromArticles(append(tc.rows, entityBound))); err == nil || !strings.Contains(err.Error(), tc.needle) {
				t.Fatalf("error=%v, want %q", err, tc.needle)
			}
		})
	}

	plan, err := DecodeSyntoIdentityPlan(syntoIdentityFixtureFromArticles([]string{
		entityBound,
		`{"id":"entityless","entity_id":null,"name":"Entityless","path":"wiki/entityless.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
		`{"id":"omitted","name":"Omitted","path":"wiki/omitted.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
	}))
	if err != nil {
		t.Fatalf("DecodeSyntoIdentityPlan() error = %v", err)
	}
	if len(plan.ByPath) != 1 || plan.ByPath["wiki/bound.md"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABE" {
		t.Fatalf("ByPath = %#v", plan.ByPath)
	}
	if _, ok := plan.ByPath["wiki/entityless.md"]; ok {
		t.Fatalf("entityless row incorrectly persisted: %#v", plan.ByPath)
	}
}

func TestDecodeSyntoIdentityPlanExcludedReservedRoots(t *testing.T) {
	plan, err := DecodeSyntoIdentityPlan(syntoIdentityFixtureFromArticles([]string{
		`{"id":"index","entity_id":null,"name":"Index","path":"wiki/index.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
		`{"id":"alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","name":"Alpha","path":"wiki/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
	}))
	if err != nil {
		t.Fatalf("DecodeSyntoIdentityPlan() error = %v", err)
	}
	if _, ok := plan.ByPath["wiki/index.md"]; ok {
		t.Fatalf("reserved root article unexpectedly present: %#v", plan.ByPath)
	}
}

func TestDecodeSyntoIdentityPlanReservedRootsParticipateInEntityUniqueness(t *testing.T) {
	_, err := DecodeSyntoIdentityPlan(syntoIdentityFixtureFromArticles([]string{
		`{"id":"index","entity_id":"` + testEntityULID + `","name":"Index","path":"wiki/index.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
		`{"id":"alpha","entity_id":"` + testEntityULID + `","name":"Alpha","path":"wiki/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`,
	}))
	if err == nil {
		t.Fatal("reserved root duplicate entity_id unexpectedly accepted")
	}
}

func TestRebuildWithSyntoIdentityRejectsMissingEntityBoundPageBeforeWrites(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{"wiki/": {}, "wiki/sources/": {}},
		reads: map[string][]byte{},
	}
	_, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
		ByPath:         map[string]string{"wiki/missing.md": testEntityULID},
		ActiveEntities: map[string]bool{},
	})
	if err == nil || len(store.writes) != 0 {
		t.Fatalf("missing entity-bound page result err=%v writes=%#v, want error and zero writes", err, store.writes)
	}
}

func syntoIdentityReleasedFixture(entityJSON string) []byte {
	entity := ""
	if entityJSON != "" {
		entity = `,"entity_id":` + entityJSON
	}
	return []byte(`{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"` + testArticleULID + `"` + entity + `,"name":"Ordinary","path":"wiki/ordinary.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[],"synthesis":[],"stats":{"article_count":1,"draft_count":0,"concept_count":0,"alias_count":0,"knowledge_item_count":0,"source_count":0,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`)
}

func syntoIdentityReleasedFixtureWithSourceConcepts(entityJSON string, sourceConcepts string) []byte {
	entity := ""
	if entityJSON != "" {
		entity = `,"entity_id":` + entityJSON
	}
	if sourceConcepts == "" {
		sourceConcepts = "[]"
	}
	return []byte(`{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"` + testArticleULID + `"` + entity + `,"name":"Ordinary","path":"wiki/ordinary.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":` + sourceConcepts + `,"synthesis":[],"stats":{"article_count":1,"draft_count":0,"concept_count":1,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`)
}

func syntoIdentityFixtureFromArticles(articles []string) []byte {
	return []byte(`{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[` + strings.Join(articles, ",") + `],"terms":[],"papers":[],"sources":[],"source_concepts":[],"synthesis":[],"stats":{"article_count":` + intToString(len(articles)) + `,"draft_count":0,"concept_count":` + intToString(len(articles)) + `,"alias_count":0,"knowledge_item_count":0,"source_count":0,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`)
}

func intToString(value int) string {
	return fmt.Sprintf("%d", value)
}

func TestRebuildWithSyntoIdentityFailsClosedOnMissingActiveEntityArticle(t *testing.T) {
	store := &fakeStore{files: map[string][]MarkdownFile{
		"wiki/":         {{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("alpha")}},
		"wiki/sources/": {},
	}, reads: map[string][]byte{}}

	if _, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
		ByPath:         map[string]string{},
		ActiveEntities: map[string]bool{"01JAZ5N7Y3K8M2Q4R6T9VWXABG": true},
	}); err == nil || !strings.Contains(err.Error(), "no entity-bound article") {
		t.Fatalf("missing active entity error = %v", err)
	}
	if len(store.writes) != 0 {
		t.Fatalf("missing active entity caused writes: %#v", store.writes)
	}
}

type fakeStore struct {
	files     map[string][]MarkdownFile
	reads     map[string][]byte
	writes    map[string][]byte
	listCalls map[string]int
}

func (s *fakeStore) ListMarkdownFiles(_ context.Context, dir string) ([]MarkdownFile, error) {
	if s.listCalls != nil {
		s.listCalls[dir]++
	}
	return append([]MarkdownFile(nil), s.files[dir]...), nil
}

func TestRebuildCollectsSourcesOnce(t *testing.T) {
	store := &fakeStore{files: map[string][]MarkdownFile{"wiki/": {}, "wiki/sources/": {{Slug: "s", Path: "wiki/sources/s.md", Data: []byte("---\nid: id\nsource_file: raw/s.md\n---")}}}, reads: map[string][]byte{}, listCalls: map[string]int{}}
	if _, err := Rebuild(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if got := store.listCalls["wiki/sources/"]; got != 1 {
		t.Fatalf("source traversals = %d, want 1", got)
	}
}

func (s *fakeStore) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	data, ok := s.reads[relPath]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeStore) WriteBytesAtomic(_ context.Context, data []byte, _, finalPath string) (string, error) {
	if s.writes == nil {
		s.writes = map[string][]byte{}
	}
	s.writes[finalPath] = append([]byte(nil), data...)
	return "digest", nil
}

func TestRebuildWritesIDMapAndConceptsJSONL(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{
					Slug: "alpha",
					Path: "wiki/alpha.md",
					Data: []byte("---\nid: concept-id\ntitle: Alpha\nsources:\n  - src-one\n---\nAlpha body"),
				},
			},
			"wiki/sources/": {
				{
					Slug: "src-one",
					Path: "wiki/sources/src-one.md",
					Data: []byte("---\nid: source-id\ntitle: Source One\n---\nSource body"),
				},
			},
		},
		reads: map[string][]byte{},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got := next.Concept["concept-id"]; got != "alpha" {
		t.Fatalf("concept id maps to %q, want alpha", got)
	}
	if got := next.Source["source-id"]; got != "src-one" {
		t.Fatalf("source id maps to %q, want src-one", got)
	}
	if _, ok := store.writes[IDMapPath]; !ok {
		t.Fatalf("missing write to %s", IDMapPath)
	}

	jsonl := strings.TrimSpace(string(store.writes[ConceptsJSONLPath]))
	var entry struct {
		Slug        string                 `json:"slug"`
		Title       string                 `json:"title"`
		Body        string                 `json:"body"`
		Frontmatter map[string]interface{} `json:"frontmatter"`
		Sources     []string               `json:"sources"`
	}
	if err := json.Unmarshal([]byte(jsonl), &entry); err != nil {
		t.Fatalf("concepts jsonl entry is not valid JSON: %v\n%s", err, jsonl)
	}
	if entry.Slug != "alpha" || entry.Title != "Alpha" || strings.TrimSpace(entry.Body) != "Alpha body" {
		t.Fatalf("entry = %+v, want alpha full cache entry", entry)
	}
	if got, ok := entry.Frontmatter["id"].(string); !ok || got != "concept-id" {
		t.Fatalf("frontmatter id = %#v, want concept-id", entry.Frontmatter["id"])
	}
	if len(entry.Sources) != 1 || entry.Sources[0] != "src-one" {
		t.Fatalf("sources = %#v, want [src-one]", entry.Sources)
	}
}

func TestRebuildWritesSyntoNestedFrontmatter(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {{
				Slug: "alpha",
				Path: "wiki/alpha.md",
				Data: []byte("---\nid: concept-id\nlineage:\n  - operation: merge\n    source_ids:\n      - source-one\n    metadata:\n      reason: rename\nrelations:\n  - type: related\n    target: beta\n---\nAlpha body"),
			}},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{},
	}

	_, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}

	var entry struct {
		Frontmatter map[string]interface{} `json:"frontmatter"`
	}
	if err := json.Unmarshal(store.writes[ConceptsJSONLPath], &entry); err != nil {
		t.Fatalf("concepts jsonl entry is not valid JSON: %v", err)
	}
	lineage := entry.Frontmatter["lineage"].([]interface{})
	lineageEntry := lineage[0].(map[string]interface{})
	if lineageEntry["operation"] != "merge" || lineageEntry["source_ids"].([]interface{})[0] != "source-one" || lineageEntry["metadata"].(map[string]interface{})["reason"] != "rename" {
		t.Fatalf("lineage = %#v, want nested values preserved", lineage)
	}
	relations := entry.Frontmatter["relations"].([]interface{})
	if relations[0].(map[string]interface{})["type"] != "related" || relations[0].(map[string]interface{})["target"] != "beta" {
		t.Fatalf("relations = %#v, want nested values preserved", relations)
	}
}

func TestIsSyntoRootPageExactPathsOnly(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "wiki/index.md", want: true},
		{path: "wiki/log.md", want: true},
		{path: "wiki/Index.md", want: false},
		{path: "wiki/Log.md", want: false},
		{path: "wiki/index2.md", want: false},
		{path: "wiki/logbook.md", want: false},
		{path: "wiki/nested/index.md", want: false},
		{path: "wiki/nested/log.md", want: false},
	}
	for _, tt := range tests {
		if got := IsSyntoRootPage(tt.path); got != tt.want {
			t.Errorf("IsSyntoRootPage(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestRebuildExcludesExactRootSyntoPagesFromIDMapAndConcepts(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{
					Slug: "index2",
					Path: "wiki/index2.md",
					Data: []byte("---\nid: index2-id\n---\nindex2"),
				},
				{
					Slug: "alpha",
					Path: "wiki/alpha.md",
					Data: []byte("---\nid: alpha-id\n---\nalpha"),
				},
				{
					Slug: "index",
					Path: "wiki/index.md",
					Data: []byte("---\nid: should-exclude-index\n---\nindex"),
				},
				{
					Slug: "log",
					Path: "wiki/log.md",
					Data: []byte("---\nid: should-exclude-log\n---\nlog"),
				},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if _, ok := next.Concept["should-exclude-index"]; ok {
		t.Fatalf("index concept not excluded from id map: %#v", next.Concept)
	}
	if _, ok := next.Concept["should-exclude-log"]; ok {
		t.Fatalf("log concept not excluded from id map: %#v", next.Concept)
	}
	if got := next.Concept["index2-id"]; got != "index2" {
		t.Fatalf("index2 concept = %q, want index2", got)
	}
	if got := next.Concept["alpha-id"]; got != "alpha" {
		t.Fatalf("alpha concept = %q, want alpha", got)
	}

	jsonl := strings.TrimSpace(string(store.writes[ConceptsJSONLPath]))
	lines := strings.Split(jsonl, "\n")
	want := map[string]bool{
		"alpha":   false,
		"index2":  false,
		"index":   false,
		"log":     false,
		"Index":   false,
		"logbook": false,
	}
	for _, line := range lines {
		var entry struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("concepts jsonl line invalid: %v", err)
		}
		if _, ok := want[entry.Slug]; ok {
			want[entry.Slug] = true
		}
	}
	if !want["alpha"] || !want["index2"] {
		t.Fatalf("concepts entries = %#v, want alpha and index2 present", want)
	}
	if want["index"] || want["log"] {
		t.Fatalf("root index/log must not be in concepts jsonl, got=%#v", want)
	}
}

func TestRebuildDoesNotExcludeOtherSimilarPageNames(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{
					Slug: "Index",
					Path: "wiki/Index.md",
					Data: []byte("---\nid: index-case-id\n---\ncase"),
				},
				{
					Slug: "logbook",
					Path: "wiki/logbook.md",
					Data: []byte("---\nid: logbook-id\n---\nlogbook"),
				},
				{
					Slug: "index",
					Path: "wiki/index.md",
					Data: []byte("---\nid: should-exclude-index\n---\nindex"),
				},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if _, ok := next.Concept["should-exclude-index"]; ok {
		t.Fatalf("root index concept unexpectedly indexed: %#v", next.Concept)
	}
	if got := next.Concept["index-case-id"]; got != "Index" {
		t.Fatalf("case variant index = %q, want Index", got)
	}
	if got := next.Concept["logbook-id"]; got != "logbook" {
		t.Fatalf("logbook concept = %q, want logbook", got)
	}

	jsonl := strings.TrimSpace(string(store.writes[ConceptsJSONLPath]))
	lines := strings.Split(jsonl, "\n")
	seen := map[string]bool{}
	for _, line := range lines {
		var entry struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("concepts jsonl line invalid: %v", err)
		}
		seen[entry.Slug] = true
	}
	if len(seen) != len(next.Concept) {
		t.Fatalf("id map/concepts jsonl cardinality mismatch: concepts=%#v jsonl=%#v", next.Concept, seen)
	}
	for _, slug := range next.Concept {
		if !seen[slug] {
			t.Fatalf("id map concept slug %q missing from concepts jsonl: concepts=%#v jsonl=%#v", slug, next.Concept, seen)
		}
	}
	if !seen["Index"] || !seen["logbook"] {
		t.Fatalf("concepts entries missing for similar non-root names: %#v", seen)
	}
}

func TestRebuildPlansAllArtifactsBeforeWritingOnNonStringNestedKey(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {{
				Slug: "alpha",
				Path: "wiki/alpha.md",
				Data: []byte("---\nid: concept-id\nmetadata:\n  123: value\n---\nAlpha body"),
			}},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{},
	}

	if _, err := Rebuild(context.Background(), store); err == nil {
		t.Fatal("Rebuild() error = nil, want non-string nested key rejection")
	}
	if len(store.writes) != 0 {
		t.Fatalf("Rebuild() writes = %#v, want zero writes", store.writes)
	}
}

func TestRebuildWithSyntoIdentityRejectsNestedUnsafeYAMLBeforeWrites(t *testing.T) {
	const frontmatter = "---\nid: " + testEntityULID + "\nmetadata:\n"
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "nested duplicate", body: "  key: one\n  key: two\n"},
		{name: "nested complex key", body: "  ? [one, two]\n  : value\n"},
		{name: "nested non-string key", body: "  7: value\n"},
		{name: "nested malformed", body: "  - [unterminated\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{
				files: map[string][]MarkdownFile{
					"wiki/":         {{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte(frontmatter + tc.body + "---\nbody\n")}},
					"wiki/sources/": {},
				},
				reads: map[string][]byte{},
			}
			_, err := RebuildWithSyntoIdentity(context.Background(), store, SyntoIdentityPlan{
				ByPath:         map[string]string{"wiki/alpha.md": testEntityULID},
				ActiveEntities: map[string]bool{testEntityULID: true},
			})
			if err == nil || len(store.writes) != 0 {
				t.Fatalf("rebuild error=%v writes=%#v, want nested validation failure before writes", err, store.writes)
			}
		})
	}
}

func TestNormalizeJSONValueRejectsNonStringMapKeys(t *testing.T) {
	_, err := normalizeJSONValue(map[interface{}]interface{}{1: "value"}, 0)
	if err == nil || !strings.Contains(err.Error(), "non-string map key") {
		t.Fatalf("normalizeJSONValue() error = %v, want non-string map key rejection", err)
	}
}

func TestNormalizeJSONValueRejectsExcessiveDepth(t *testing.T) {
	value := interface{}("leaf")
	for i := 0; i <= maxJSONNormalizationDepth; i++ {
		value = []interface{}{value}
	}
	_, err := normalizeJSONValue(value, 0)
	if err == nil || !strings.Contains(err.Error(), "maximum nesting depth") {
		t.Fatalf("normalizeJSONValue() error = %v, want depth limit rejection", err)
	}
}

func TestNormalizeJSONValueDoesNotLeakNonStringMapKey(t *testing.T) {
	const sentinel = "confidential-sentinel"
	key := struct{ Value string }{Value: sentinel}
	_, err := normalizeJSONValue(map[interface{}]interface{}{key: "value"}, 0)
	if err == nil {
		t.Fatal("normalizeJSONValue() error = nil, want non-string map key rejection")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("normalizeJSONValue() error = %q, must not contain sentinel", err)
	}
}

func TestRebuildPreservesRedirects(t *testing.T) {
	old := IDMap{
		Concept:   map[string]string{"same-id": "old-alpha"},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{Slug: "new-alpha", Path: "wiki/new-alpha.md", Data: []byte("---\nid: same-id\ntitle: Alpha\n---\nBody")},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{IDMapPath: oldJSON},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got := next.Redirects["same-id"]; len(got) != 1 || got[0] != "old-alpha" {
		t.Fatalf("redirects = %#v, want old-alpha", next.Redirects)
	}
}

func TestRebuildPreservesDormantConceptsAndOwnedEntityMappings(t *testing.T) {
	old := IDMap{
		Concept:         map[string]string{"stable-alpha": "alpha"},
		DormantConcept:  map[string]string{"stable-beta": "beta"},
		ConceptEntityID: map[string]string{"stable-alpha": "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "stable-beta": "01JAZ5N7Y3K8M2Q4R6T9VWXABD", "orphan": "entity-orphan"},
		Source:          map[string]string{},
		Redirects:       map[string][]string{},
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/":         {{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("---\nid: stable-alpha\n---\nAlpha")}},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{IDMapPath: oldJSON},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if next.DormantConcept["stable-beta"] != "beta" {
		t.Fatalf("dormant concept = %#v, want stable-beta -> beta", next.DormantConcept)
	}
	if next.ConceptEntityID["stable-alpha"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABC" || next.ConceptEntityID["stable-beta"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABD" {
		t.Fatalf("owned entity mappings = %#v", next.ConceptEntityID)
	}
	if _, ok := next.ConceptEntityID["orphan"]; ok {
		t.Fatalf("orphan entity mapping was retained: %#v", next.ConceptEntityID)
	}
}

func TestRebuildFailsClosedOnLifecycleMappingCollisions(t *testing.T) {
	tests := []struct {
		name string
		old  IDMap
	}{
		{
			name: "active dormant slug",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-beta": "alpha"},
				ConceptEntityID: map[string]string{"stable-beta": "01JAZ5N7Y3K8M2Q4R6T9VWXABD"},
			},
		},
		{
			name: "active dormant id",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-alpha": "beta"},
				ConceptEntityID: map[string]string{"stable-alpha": "01JAZ5N7Y3K8M2Q4R6T9VWXABC"},
			},
		},
		{
			name: "dormant slug collision",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-a": "beta", "stable-b": "beta"},
				ConceptEntityID: map[string]string{"stable-a": "entity-a", "stable-b": "entity-b"},
			},
		},
		{
			name: "retained entity collision",
			old: IDMap{
				DormantConcept:  map[string]string{"stable-a": "alpha", "stable-b": "beta"},
				ConceptEntityID: map[string]string{"stable-a": "same", "stable-b": "same"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldJSON, err := json.Marshal(tc.old)
			if err != nil {
				t.Fatal(err)
			}
			store := &fakeStore{
				files: map[string][]MarkdownFile{
					"wiki/":         {{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("---\nid: stable-alpha\n---\nAlpha")}},
					"wiki/sources/": {},
				},
				reads: map[string][]byte{IDMapPath: oldJSON},
			}
			if _, err := Rebuild(context.Background(), store); err == nil {
				t.Fatal("Rebuild() error = nil, want fail-closed lifecycle mapping rejection")
			}
			if _, ok := store.writes[IDMapPath]; ok {
				t.Fatal("Rebuild() wrote id_map after lifecycle validation failure")
			}
		})
	}
}

func TestRebuildFailsClosedOnMalformedLifecycleMapping(t *testing.T) {
	old := IDMap{
		DormantConcept:  map[string]string{"../stable": "beta"},
		ConceptEntityID: map[string]string{"../stable": "01JAZ5N7Y3K8M2Q4R6T9VWXABD"},
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		files: map[string][]MarkdownFile{"wiki/": {}, "wiki/sources/": {}},
		reads: map[string][]byte{IDMapPath: oldJSON},
	}
	if _, err := Rebuild(context.Background(), store); err == nil {
		t.Fatal("Rebuild() error = nil, want malformed lifecycle mapping rejection")
	}
	if _, ok := store.writes[IDMapPath]; ok {
		t.Fatal("Rebuild() wrote id_map after malformed lifecycle validation failure")
	}
}
