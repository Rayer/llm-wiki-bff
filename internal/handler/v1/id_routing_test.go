package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

const testSyntoConceptULID = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"

func TestBuildDualIDMapPrefersConceptForDuplicateSlug(t *testing.T) {
	source := idMap{
		Concept: map[string]string{
			"a3f7b2c01d9d": "shared-slug",
		},
		Source: map[string]string{
			"c5d9e3f1a028": "shared-slug",
		},
	}

	dual := buildDualIDMap(source)

	if got := dual.byID["a3f7b2c01d9d"]; got.Type != "concept" || got.Slug != "shared-slug" {
		t.Fatalf("concept id route = %#v", got)
	}
	if got := dual.byID["c5d9e3f1a028"]; got.Type != "source" || got.Slug != "shared-slug" {
		t.Fatalf("source id route = %#v", got)
	}
	entries := dual.bySlug["shared-slug"]
	if len(entries) != 2 {
		t.Fatalf("shared slug entries = %#v, want 2 entries", entries)
	}
	if entries[0].Type != "concept" || entries[1].Type != "source" {
		t.Fatalf("shared slug order = %#v, want concept then source", entries)
	}
}

func TestRewriteWikilinksEmitsCanonicalTargetsWithSlugLabels(t *testing.T) {
	dual := buildDualIDMap(idMap{
		Concept: map[string]string{
			"a3f7b2c01d9d": "alpha",
			"b7e2c9a4d113": "shared",
		},
		Source: map[string]string{
			"c5d9e3f1a028": "source-one",
			"d4c8f9b0a177": "shared",
		},
	})
	input := "[[alpha]] [[alpha|Alias]] [[alpha#part|Section]] [[source-one]] [[missing]] [[concepts/a3f7b2c01d9d-alpha|Already]] [[sources/c5d9e3f1a028-source-one]] [[shared|Shared]]"

	got := rewriteWikilinks(input, dual)

	want := "[[concepts/a3f7b2c01d9d-alpha|alpha]] [[concepts/a3f7b2c01d9d-alpha|Alias]] [[concepts/a3f7b2c01d9d-alpha#part|Section]] [[sources/c5d9e3f1a028-source-one|source-one]] [[missing]] [[concepts/a3f7b2c01d9d-alpha|Already]] [[sources/c5d9e3f1a028-source-one|source-one]] [[concepts/b7e2c9a4d113-shared|Shared]]"
	if got != want {
		t.Fatalf("rewriteWikilinks = %q, want %q", got, want)
	}
}

func TestIDRouteRedirectStatusIsTemporary(t *testing.T) {
	if idRouteRedirectStatus != http.StatusFound {
		t.Fatalf("idRouteRedirectStatus = %d, want %d", idRouteRedirectStatus, http.StatusFound)
	}
}

func TestParseIDSlug(t *testing.T) {
	id, slug, ok := parseIDSlug("a3f7b2c01d9d-alpha")
	if !ok || id != "a3f7b2c01d9d" || slug != "alpha" {
		t.Fatalf("parseIDSlug valid = (%q, %q, %v)", id, slug, ok)
	}
	if _, _, ok := parseIDSlug("a3f7b2c01d9d"); ok {
		t.Fatal("parseIDSlug accepted id-only path")
	}
	if _, _, ok := parseIDSlug("not-an-id-alpha"); ok {
		t.Fatal("parseIDSlug accepted invalid id")
	}
}

func TestParseIDSlugAcceptsReleasedSyntoULID(t *testing.T) {
	if len(testSyntoConceptULID) != 26 {
		t.Fatalf("test ULID length = %d, want 26", len(testSyntoConceptULID))
	}
	id, slug, ok := parseIDSlug(testSyntoConceptULID + "-alpha")
	if !ok || id != testSyntoConceptULID || slug != "alpha" {
		t.Fatalf("parseIDSlug ULID = (%q, %q, %v)", id, slug, ok)
	}
}

func TestCanonicalIDRouteLooksUpULIDSourceOnlyWhenMapped(t *testing.T) {
	const sourceULID = "01JAZ5N7Y3K8M2Q4R6T9VWXABD"
	dual := buildDualIDMap(idMap{
		Source: map[string]string{sourceULID: "source-one"},
	})
	target, ok := canonicalIDRoute("source", sourceULID, dual)
	if !ok || target != "/sources/"+sourceULID+"-source-one" {
		t.Fatalf("mapped ULID source route = (%q, %v)", target, ok)
	}
}

func TestIDFromPathValueRejectsInvalidULIDGrammar(t *testing.T) {
	invalid := []string{
		"01JAZ5N7Y3K8M2Q4R6T9VWXAB",          // 25 characters
		"01JAZ5N7Y3K8M2Q4R6T9VWXABCD",        // 27 characters
		"01JAZ5N7Y3K8M2Q4R6T9VWXABCI",        // forbidden I
		"01JAZ5N7Y3K8M2Q4R6T9VWXABCL",        // forbidden L
		"01JAZ5N7Y3K8M2Q4R6T9VWXABCO",        // forbidden O
		"01JAZ5N7Y3K8M2Q4R6T9VWXABCU",        // forbidden U
		"01jaz5n7y3k8m2q4r6t9vw xabc",        // lowercase and space
		"01JAZ5N7Y3K8M2Q4R6T9VWXABC!",        // punctuation
		"01JAZ5N7Y3K8M2Q4R6T9VWXABC/alpha",   // slash
		"01JAZ5N7Y3K8M2Q4R6T9VWXABC%2Falpha", // percent ambiguity
		"entity-alpha",                       // arbitrary ID
		"8JAZ5N7Y3K8M2Q4R6T9VWXABC",          // ULID overflow
		"Z1JAZ5N7Y3K8M2Q4R6T9VWXABC",         // ULID overflow
	}
	for _, value := range invalid {
		if _, _, ok := idFromPathValue(value); ok {
			t.Errorf("idFromPathValue accepted invalid route value %q", value)
		}
		if _, _, ok := parseIDSlug(value + "-alpha"); ok {
			t.Errorf("parseIDSlug accepted invalid route value %q", value)
		}
	}
}

func TestCanonicalIDRouteSupportsSyntoULIDAndLegacyRedirect(t *testing.T) {
	dual := buildDualIDMap(idMap{
		Concept: map[string]string{testSyntoConceptULID: "alpha"},
		Source:  map[string]string{"c5d9e3f1a028": "source-one"},
		IDRedirects: map[string]string{
			"a3f7b2c01d9d": testSyntoConceptULID,
		},
	})

	tests := []struct {
		name        string
		currentType string
		idSlug      string
		want        string
	}{
		{name: "id only", currentType: "concept", idSlug: testSyntoConceptULID, want: "/concepts/" + testSyntoConceptULID + "-alpha"},
		{name: "type mismatch", currentType: "source", idSlug: testSyntoConceptULID + "-wrong", want: "/concepts/" + testSyntoConceptULID + "-alpha"},
		{name: "legacy redirect", currentType: "concept", idSlug: "a3f7b2c01d9d-old-alpha", want: "/concepts/" + testSyntoConceptULID + "-alpha"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := canonicalIDRoute(tt.currentType, tt.idSlug, dual)
			if !ok || got != tt.want {
				t.Fatalf("canonicalIDRoute = (%q, %v), want (%q, true)", got, ok, tt.want)
			}
		})
	}
}

func TestRewriteWikilinksUsesSyntoULIDAndPreservesLegacySourceGrammar(t *testing.T) {
	dual := buildDualIDMap(idMap{
		Concept: map[string]string{testSyntoConceptULID: "alpha"},
		Source:  map[string]string{"c5d9e3f1a028": "source-one"},
	})
	input := "[[alpha]] [[concepts/" + testSyntoConceptULID + "-alpha|Already]] [[sources/c5d9e3f1a028-source-one]]"
	want := "[[concepts/" + testSyntoConceptULID + "-alpha|alpha]] [[concepts/" + testSyntoConceptULID + "-alpha|Already]] [[sources/c5d9e3f1a028-source-one|source-one]]"
	if got := rewriteWikilinks(input, dual); got != want {
		t.Fatalf("rewriteWikilinks = %q, want %q", got, want)
	}
}

func TestGetConceptServesCanonicalSyntoULIDAndRedirectsLegacyID(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "users", "user", "projects", "project")
	for _, rel := range []string{"cache", "wiki"} {
		if err := os.MkdirAll(filepath.Join(projectRoot, rel), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRouteTestFile(t, projectRoot, "cache/id_map.json", []byte(`{"concept":{"01JAZ5N7Y3K8M2Q4R6T9VWXABC":"alpha"},"source":{},"redirects":{},"id_redirects":{"a3f7b2c01d9d":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}}`))
	writeRouteTestFile(t, projectRoot, "wiki/alpha.md", []byte("---\nstatus: published\n---\nAlpha body\n"))
	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantHeader string
		wantID     string
	}{
		{name: "canonical ULID", path: "/api/v1/concepts/" + testSyntoConceptULID + "-alpha", wantStatus: http.StatusOK, wantID: testSyntoConceptULID},
		{name: "legacy ID redirect", path: "/api/v1/concepts/a3f7b2c01d9d-old-alpha", wantStatus: http.StatusFound, wantHeader: "/api/v1/concepts/" + testSyntoConceptULID + "-alpha"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, tt.path, nil)
			c.Params = gin.Params{{Key: "id", Value: filepath.Base(tt.path)}}
			c.Set("userID", "user")
			c.Set("projectID", "project")
			h.GetConcept(c)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantHeader != "" && recorder.Header().Get("Location") != tt.wantHeader {
				t.Fatalf("Location = %q, want %q", recorder.Header().Get("Location"), tt.wantHeader)
			}
			if tt.wantID != "" {
				var response handler.ConceptDetailResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response.ID != tt.wantID {
					t.Fatalf("response ID = %q, want %q", response.ID, tt.wantID)
				}
			}
		})
	}
}

func writeRouteTestFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalIDRouteRedirectsSlugAndTypeMismatches(t *testing.T) {
	dual := buildDualIDMap(idMap{
		Concept: map[string]string{"a3f7b2c01d9d": "alpha"},
		Source:  map[string]string{"c5d9e3f1a028": "source-one"},
	})

	target, ok := canonicalIDRoute("source", "a3f7b2c01d9d-alpha", dual)
	if !ok || target != "/concepts/a3f7b2c01d9d-alpha" {
		t.Fatalf("type mismatch route = %q, %v", target, ok)
	}
	target, ok = canonicalIDRoute("concept", "a3f7b2c01d9d-old-alpha", dual)
	if !ok || target != "/concepts/a3f7b2c01d9d-alpha" {
		t.Fatalf("slug mismatch route = %q, %v", target, ok)
	}
	target, ok = canonicalIDRoute("source", "c5d9e3f1a028", dual)
	if !ok || target != "/sources/c5d9e3f1a028-source-one" {
		t.Fatalf("id-only route = %q, %v", target, ok)
	}
	if target, ok = canonicalIDRoute("concept", "ffffffffffff-missing", dual); ok || target != "" {
		t.Fatalf("missing id route = %q, %v", target, ok)
	}
}

func TestLoadDualIDMapReturnsNotFoundForMissingFile(t *testing.T) {
	_, err := loadDualIDMap(context.Background(), missingIDMapStore{})
	if !errors.Is(err, errIDMapNotFound) {
		t.Fatalf("loadDualIDMap error = %v, want errIDMapNotFound", err)
	}
}

type missingIDMapStore struct{}

func (missingIDMapStore) ListMarkdownFiles(context.Context, string) ([]gcs.MarkdownFile, error) {
	return nil, nil
}

func (missingIDMapStore) ReadFile(context.Context, string) ([]byte, error) {
	return nil, storage.ErrObjectNotExist
}
