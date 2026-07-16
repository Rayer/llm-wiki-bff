package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type fakeRegistrationGate struct {
	enabled bool
	err     error
	called  bool
}

func (f *fakeRegistrationGate) IsRegistrationEnabled(context.Context) (bool, error) {
	f.called = true
	return f.enabled, f.err
}

func TestRegisterHandler_DisabledReturns403(t *testing.T) {
	gin.SetMode(gin.TestMode)

	gate := &fakeRegistrationGate{enabled: false}
	router := gin.New()
	router.POST("/register", RegisterHandler(nil, "test-secret", gate))

	body := `{"email":"new@example.com","password":"password123"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !gate.called {
		t.Fatal("registration gate was not consulted")
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "registration is disabled" {
		t.Fatalf("error = %q, want %q", resp["error"], "registration is disabled")
	}
}

func TestRegisterHandler_EnabledPassesGate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	gate := &fakeRegistrationGate{enabled: true}
	router := gin.New()
	router.POST("/register", RegisterHandler(nil, "test-secret", gate))

	body := `{"email":"new@example.com","password":"short"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if !gate.called {
		t.Fatal("registration gate was not consulted")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s; want 400 after gate for short password", rec.Code, rec.Body.String())
	}
}