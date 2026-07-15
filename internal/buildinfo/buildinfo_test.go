package buildinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestCurrentUsesLocalBuildDefaults(t *testing.T) {
	withBuildVariables(t, "dev", "unknown", "unknown", "", "unknown")
	t.Setenv("K_SERVICE", "")
	t.Setenv("K_REVISION", "")

	if got, want := Current(), (Info{
		ProductVersion: "dev",
		Commit:         "unknown",
		Branch:         "unknown",
		Tag:            "",
		ImageTag:       "unknown",
		Service:        "unknown",
		Revision:       "unknown",
	}); got != want {
		t.Fatalf("Current() = %#v, want %#v", got, want)
	}
}

func TestCurrentUsesInjectedBuildMetadataAndCloudRunIdentity(t *testing.T) {
	withBuildVariables(t, "1.0.0", "0123456789abcdef0123456789abcdef01234567", "develop/1.0", "v1.0.0", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("K_SERVICE", "llm-wiki-bff-dev")
	t.Setenv("K_REVISION", "llm-wiki-bff-dev-00042-abc")

	if got, want := Current(), (Info{
		ProductVersion: "1.0.0",
		Commit:         "0123456789abcdef0123456789abcdef01234567",
		Branch:         "develop/1.0",
		Tag:            "v1.0.0",
		ImageTag:       "0123456789abcdef0123456789abcdef01234567",
		Service:        "llm-wiki-bff-dev",
		Revision:       "llm-wiki-bff-dev-00042-abc",
	}); got != want {
		t.Fatalf("Current() = %#v, want %#v", got, want)
	}
}

func TestVersionHandlerReturnsAllowlistedNoStoreJSON(t *testing.T) {
	withBuildVariables(t, "1.0.0", "0123456789abcdef0123456789abcdef01234567", "develop/1.0", "v1.0.0", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("K_SERVICE", "llm-wiki-bff-dev")
	t.Setenv("K_REVISION", "llm-wiki-bff-dev-00042-abc")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/public/version", Handler())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/version", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{
		"product_version": "1.0.0",
		"commit":          "0123456789abcdef0123456789abcdef01234567",
		"branch":          "develop/1.0",
		"tag":             "v1.0.0",
		"image_tag":       "0123456789abcdef0123456789abcdef01234567",
		"service":         "llm-wiki-bff-dev",
		"revision":        "llm-wiki-bff-dev-00042-abc",
	}
	if len(body) != len(want) {
		t.Fatalf("response fields = %#v, want exactly %#v", body, want)
	}
	for key, value := range want {
		if got := body[key]; got != value {
			t.Errorf("response[%q] = %q, want %q", key, got, value)
		}
	}
}

func withBuildVariables(t *testing.T, version, commit, branch, tag, imageTag string) {
	t.Helper()
	originalVersion, originalCommit, originalBranch, originalTag, originalImageTag := ProductVersion, GitSHA, GitBranch, GitTag, ImageTag
	ProductVersion, GitSHA, GitBranch, GitTag, ImageTag = version, commit, branch, tag, imageTag
	t.Cleanup(func() {
		ProductVersion, GitSHA, GitBranch, GitTag, ImageTag = originalVersion, originalCommit, originalBranch, originalTag, originalImageTag
	})
}
