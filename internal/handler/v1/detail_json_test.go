package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

// nestedFrontmatterMarkdown reproduces the yaml.v2 map[interface{}]interface{}
// shape that previously caused LWC-219 empty HTTP 200 bodies.
const nestedFrontmatterMarkdown = `---
id: 01JAZ5N7Y3K8M2Q4R6T9VWXABC
title: YouTrack
status: published
lineage:
  - operation: merge
    source_ids:
      - source-one
    metadata:
      reason: rename
relations:
  - type: related
    target: beta
    meta:
      nested: true
tags:
  - tracker
  - ops
count: 3
enabled: true
---
Body about YouTrack.
`

const nestedSourceMarkdown = `---
id: 01JAZ5N7Y3K8M2Q4R6T9VWXABD
title: Nested Source
source_file: raw/nested-source.md
lineage:
  - operation: import
    metadata:
      reason: seed
---
Source body.
`

func writeDetailJSONFixture(t *testing.T, root, user, project string) {
	t.Helper()
	projectRoot := filepath.Join(root, "users", user, "projects", project)
	for _, rel := range []string{"cache", "wiki", "wiki/sources", "raw"} {
		if err := os.MkdirAll(filepath.Join(projectRoot, rel), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	idMap := `{"concept":{"01JAZ5N7Y3K8M2Q4R6T9VWXABC":"YouTrack"},"source":{"01JAZ5N7Y3K8M2Q4R6T9VWXABD":"Nested Source"},"redirects":{},"id_redirects":{}}`
	mustWriteDetailFile(t, projectRoot, "cache/id_map.json", []byte(idMap))
	mustWriteDetailFile(t, projectRoot, "wiki/YouTrack.md", []byte(nestedFrontmatterMarkdown))
	mustWriteDetailFile(t, projectRoot, "wiki/sources/Nested Source.md", []byte(nestedSourceMarkdown))
	mustWriteDetailFile(t, projectRoot, "raw/nested-source.md", []byte("raw body\n"))
}

func mustWriteDetailFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGetConceptNestedFrontmatterReturnsValidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeDetailJSONFixture(t, root, "user", "project")
	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)

	for _, path := range []string{"YouTrack", "01JAZ5N7Y3K8M2Q4R6T9VWXABC-YouTrack"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/concepts/"+path, nil)
			c.Params = gin.Params{{Key: "id", Value: path}}
			c.Set("userID", "user")
			c.Set("projectID", "project")

			h.GetConcept(c)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Body.Len() == 0 {
				t.Fatal("empty body with HTTP 200 (LWC-219 regression)")
			}
			var response handler.ConceptDetailResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v body=%q", err, recorder.Body.String())
			}
			if response.Slug == "" && path == "YouTrack" {
				// slug path keeps request slug; ID path uses canonical slug from id map.
			}
			assertNestedFrontmatter(t, response.Frontmatter)
			if response.Body == "" {
				t.Fatal("body empty")
			}
		})
	}
}

func TestGetSourceNestedFrontmatterReturnsValidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	writeDetailJSONFixture(t, root, "user", "project")
	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)

	for _, path := range []string{"Nested Source", "01JAZ5N7Y3K8M2Q4R6T9VWXABD-Nested Source"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/sources/"+url.PathEscape(path), nil)
			c.Params = gin.Params{{Key: "id", Value: path}}
			c.Set("userID", "user")
			c.Set("projectID", "project")

			h.GetSource(c)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Body.Len() == 0 {
				t.Fatal("empty body with HTTP 200 (LWC-219 regression)")
			}
			var response handler.SourceDetailResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v body=%q", err, recorder.Body.String())
			}
			assertNestedFrontmatter(t, response.Frontmatter)
		})
	}
}

func TestWriteJSONNeverReturns200EmptyBodyOnMarshalFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/concepts/x", nil)

	// Unnormalizable payload: map[interface{}]interface{} is rejected by encoding/json.
	writeJSON(c, http.StatusOK, map[string]interface{}{
		"frontmatter": map[interface{}]interface{}{"k": "v"},
	})

	if recorder.Code == http.StatusOK && recorder.Body.Len() == 0 {
		t.Fatal("marshal failure produced HTTP 200 with empty body")
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	var errBody handler.ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error != "response serialization failed" {
		t.Fatalf("error = %q", errBody.Error)
	}
}

func TestParseFrontmatterJSONMakesNestedMapsJSONSafe(t *testing.T) {
	fm, body, err := parseFrontmatterJSON(nestedFrontmatterMarkdown)
	if err != nil {
		t.Fatalf("parseFrontmatterJSON() error = %v", err)
	}
	if body == "" {
		t.Fatal("body empty")
	}
	if _, err := json.Marshal(fm); err != nil {
		t.Fatalf("json.Marshal(frontmatter) error = %v", err)
	}
	assertNestedFrontmatter(t, fm)
}

func assertNestedFrontmatter(t *testing.T, fm map[string]interface{}) {
	t.Helper()
	if fm == nil {
		t.Fatal("frontmatter is nil")
	}
	lineage, ok := fm["lineage"].([]interface{})
	if !ok || len(lineage) == 0 {
		t.Fatalf("lineage = %#v", fm["lineage"])
	}
	entry, ok := lineage[0].(map[string]interface{})
	if !ok {
		t.Fatalf("lineage[0] type = %T, want map[string]interface{}", lineage[0])
	}
	meta, ok := entry["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("metadata type = %T", entry["metadata"])
	}
	if meta["reason"] != "rename" && meta["reason"] != "seed" {
		t.Fatalf("metadata.reason = %#v", meta["reason"])
	}
}
