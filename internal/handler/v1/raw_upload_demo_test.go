package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRawUploadBlocksDemoUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	h.SetPipelineQuotaConfig(2, 3600, 1, []string{"demo-user"})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/raw/upload", nil)
	c.Set("userID", "demo-user")
	c.Set("projectID", "proj-1")

	h.RawUpload(c)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "demo users cannot upload raw files" {
		t.Fatalf("error = %#v", body["error"])
	}
}
