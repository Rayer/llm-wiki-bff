package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/config"
	handlerv1 "github.com/rayer/llm-wiki-bff/internal/handler/v1"
	"github.com/rayer/llm-wiki-bff/internal/syssettings"
)

func TestProductionRouterKeepsAuthCompatibilityLane(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := newProductionRouter(
		config.Config{DevJWT: true, JWTSecret: "test-secret"},
		true,
		nil,
		nil,
		handlerv1.New(nil, nil, nil, nil, nil, nil),
		&syssettings.FakeStore{Enabled: true},
		nil,
	)

	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/register", "/api/v1/auth/refresh", "/api/v1/auth/logout"} {
		if !hasRoute(router, http.MethodPost, path) {
			t.Fatalf("BFF production router is missing compatibility route POST %s", path)
		}
	}

	request := func(path string, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)
		return recorder
	}
	if got := request("/api/v1/auth/login", `{"email":"demo@llm-wiki.dev","password":"demo123456"}`).Code; got != http.StatusOK {
		t.Fatalf("local compatibility login status = %d, want %d", got, http.StatusOK)
	}
	if got := request("/api/v1/auth/register", `{}`).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("local compatibility register status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := request("/api/v1/auth/refresh", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("local compatibility refresh status = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := request("/api/v1/auth/logout", "").Code; got != http.StatusOK {
		t.Fatalf("local compatibility logout status = %d, want %d", got, http.StatusOK)
	}
	if got := request("/api/v1/auth/login", `{}`).Code; got != http.StatusBadRequest {
		t.Fatalf("small malformed local compatibility login status = %d, want %d", got, http.StatusBadRequest)
	}
	if got := request("/api/v1/auth/login", `{"email":"demo@llm-wiki.dev","password":"`+strings.Repeat("x", 64<<10)+`"}`).Code; got != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized local compatibility login status = %d, want %d", got, http.StatusRequestEntityTooLarge)
	}

	for i := 0; i < 11; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewBufferString(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("CF-Connecting-IP", "203.0.113.258")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if i == 10 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("local compatibility login request %d status = %d, want %d", i+1, rec.Code, http.StatusTooManyRequests)
		}
	}

	unavailable := newProductionRouter(
		config.Config{JWTSecret: "test-secret", AllowedOrigins: []string{"https://frontend.example"}},
		false,
		nil,
		nil,
		handlerv1.New(nil, nil, nil, nil, nil, nil),
		&syssettings.FakeStore{Enabled: true},
		nil,
	)
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/register", "/api/v1/auth/refresh"} {
		if got := servePost(unavailable, path).Code; got != http.StatusServiceUnavailable {
			t.Fatalf("unavailable compatibility %s status = %d, want %d", path, got, http.StatusServiceUnavailable)
		}
	}
	logout := servePost(unavailable, "/api/v1/auth/logout")
	if logout.Code != http.StatusOK {
		t.Fatalf("unavailable compatibility logout status = %d, want %d", logout.Code, http.StatusOK)
	}
	cookies := logout.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("legacy compatibility logout returned %d cookies, want one", len(cookies))
	}
	if cookies[0].Name != "refresh_token" || cookies[0].Domain != "rayer.idv.tw" || cookies[0].Path != "/" || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].MaxAge != -1 {
		t.Fatalf("legacy compatibility logout cookie attributes: count=%d name=%q domain=%q path=%q secure=%v httpOnly=%v sameSite=%v maxAge=%d", len(cookies), cookies[0].Name, cookies[0].Domain, cookies[0].Path, cookies[0].Secure, cookies[0].HttpOnly, cookies[0].SameSite, cookies[0].MaxAge)
	}

	for _, path := range []string{"/api/v1/public/config", "/api/v1/public/version"} {
		if !hasRoute(router, http.MethodGet, path) {
			t.Fatalf("BFF production router is missing GET %s", path)
		}
	}
}

func servePost(router *gin.Engine, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
	return recorder
}

func hasRoute(router *gin.Engine, method, path string) bool {
	for _, route := range router.Routes() {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}
