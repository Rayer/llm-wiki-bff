package v1

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestRawListReturnsFilesWithIngestedFromArtifact(t *testing.T) {
	root := t.TempDir()
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/raw/seed.md", "seed")
	sum := sha256Hex("seed")
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/cache/raw_status.json", `{"version":1,"files":{"seed.md":{"path":"raw/seed.md","sha256":"`+sum+`","olw_status":"ingested","ingested":true,"error":""}}}`)

	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/raw", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.RawList(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Files []struct {
			Name     string `json:"name"`
			SHA256   string `json:"sha256"`
			Ingested bool   `json:"ingested"`
		} `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 1 || body.Files[0].Name != "seed.md" || body.Files[0].SHA256 != sum || !body.Files[0].Ingested {
		t.Fatalf("body = %#v", body)
	}
}

func TestRawListMissingStatusMarksAllUningested(t *testing.T) {
	root := t.TempDir()
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/raw/seed.md", "seed")

	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/raw", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.RawList(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Files []struct {
			Ingested bool `json:"ingested"`
		} `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 1 || body.Files[0].Ingested {
		t.Fatalf("body = %#v, want one uningested file", body)
	}
}

func TestRawListMalformedStatusReturns500(t *testing.T) {
	root := t.TempDir()
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/raw/seed.md", "seed")
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/cache/raw_status.json", `{"files":`)

	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/raw", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.RawList(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func mustWriteHandlerFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}
