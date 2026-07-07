package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLocalDevLoginAcceptsFrontendDemoAccount(t *testing.T) {
	resetRefreshTokensForTest()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/auth/login", LocalDevLoginHandler("test-secret"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{"email":"demo@llm-wiki.dev","password":"demo123456"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body LoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.AccessToken == "" {
		t.Fatal("access token is empty")
	}
	if body.User.ID != "local-user" || body.User.Email != "demo@llm-wiki.dev" {
		t.Fatalf("user = %+v, want local demo user", body.User)
	}
	cookie := findRefreshCookie(t, rec)
	if cookie.Domain != "" || cookie.Secure {
		t.Fatalf("local cookie domain=%q secure=%v, want localhost-friendly cookie", cookie.Domain, cookie.Secure)
	}
}

func TestLocalDevRefreshReturnsLocalUser(t *testing.T) {
	resetRefreshTokensForTest()
	token, err := GenerateRefreshToken("local-user", "test-secret")
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/auth/refresh", LocalDevRefreshHandler("test-secret"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshTokenCookieName, Value: token})
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body RefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.AccessToken == "" || body.User.ID != "local-user" || body.User.Email != "demo@llm-wiki.dev" {
		t.Fatalf("body = %+v, want local user refresh response", body)
	}
}
