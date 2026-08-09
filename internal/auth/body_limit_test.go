package auth

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type chunkedReader struct {
	data []byte
	pos  int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > 13 {
		n = 13
	}
	if remaining := len(r.data) - r.pos; n > remaining {
		n = remaining
	}

	copied := copy(p, r.data[r.pos:r.pos+n])
	r.pos += copied
	return copied, nil
}

func makeUnknownLengthRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/compat", &chunkedReader{data: body})
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func makeJSONBodyOfSize(size int) string {
	const overhead = len(`{"x":""}`)
	if size < overhead {
		panic("size too small for json payload")
	}
	return `{"x":"` + strings.Repeat("x", size-overhead) + `"}`
}

func TestCompatibilityBodyLimitUnknownLengthBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(CompatibilityBodyLimit())
	router.POST("/compat", func(c *gin.Context) {
		var payload map[string]any
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})

	smallMalformedReq := makeUnknownLengthRequest(t, []byte("{"))
	smallMalformedResp := httptest.NewRecorder()
	router.ServeHTTP(smallMalformedResp, smallMalformedReq)
	if smallMalformedResp.Code != http.StatusBadRequest {
		t.Fatalf("invalid unknown-length payload status = %d, want %d", smallMalformedResp.Code, http.StatusBadRequest)
	}

	exactReq := makeUnknownLengthRequest(t, []byte(makeJSONBodyOfSize(int(MaxRequestBodyBytes))))
	exactResp := httptest.NewRecorder()
	router.ServeHTTP(exactResp, exactReq)
	if exactResp.Code != http.StatusOK {
		t.Fatalf("exact-limit unknown-length payload status = %d, want %d", exactResp.Code, http.StatusOK)
	}

	overPayload := bytes.Repeat([]byte{'x'}, int(MaxRequestBodyBytes)+1)
	overReq := makeUnknownLengthRequest(t, overPayload)
	overResp := httptest.NewRecorder()
	router.ServeHTTP(overResp, overReq)
	if overResp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized unknown-length payload status = %d, want %d", overResp.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestRequestBodyLimitPreservesLegacyUnknownLengthBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(RequestBodyLimit())
	router.POST("/compat", func(c *gin.Context) {
		var body bytes.Buffer
		if _, err := body.ReadFrom(c.Request.Body); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		if body.Len() != 128 {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Status(http.StatusOK)
	})

	req := makeUnknownLengthRequest(t, bytes.Repeat([]byte{'x'}, 128))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("RequestBodyLimit unknown-length request status = %d, want %d", resp.Code, http.StatusOK)
	}
}
