package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/syssettings"
)

func TestProductionRouterExposesOnlyAuthPublicSurface(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(config.Config{DevJWT: true, JWTSecret: "test-secret"}, true, nil, &syssettings.FakeStore{Enabled: true})

	want := map[string]bool{
		http.MethodPost + " /api/v1/auth/register": true,
		http.MethodPost + " /api/v1/auth/login":    true,
		http.MethodPost + " /api/v1/auth/refresh":  true,
		http.MethodPost + " /api/v1/auth/logout":   true,
		http.MethodGet + " /healthz":               true,
		http.MethodGet + " /api/v1/public/version": true,
	}

	got := make(map[string]bool)
	for _, route := range router.Routes() {
		got[route.Method+" "+route.Path] = true
	}
	if len(got) != len(want) {
		t.Fatalf("Auth production routes = %#v, want exactly %#v", got, want)
	}
	for route := range want {
		if !got[route] {
			t.Errorf("Auth production router is missing %s", route)
		}
	}
	for route := range got {
		if !want[route] {
			t.Errorf("Auth production router exposed unapproved route %s", route)
		}
	}
}

func TestProductionRouterWiresLocalAuthHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(config.Config{DevJWT: true, JWTSecret: "test-secret"}, true, nil, &syssettings.FakeStore{Enabled: true})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://localhost:8081/api/v1/auth/login", bytes.NewBufferString(`{"email":"demo@llm-wiki.dev","password":"demo123456"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("local login status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var loginResponse struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &loginResponse); err != nil {
		t.Fatalf("decode local login response: %v", err)
	}
	if loginResponse.AccessToken == "" {
		t.Fatal("local login response did not contain an access token")
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "http://localhost:8081/api/v1/auth/register", bytes.NewBufferString(`{"email":"new@example.com","password":"password123"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("local register status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestProductionRouterUsesHostOnlyRefreshCookiePolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(config.Config{
		JWTSecret:      "test-secret",
		AllowedHosts:   []string{"auth.example.test"},
		AllowedOrigins: []string{"https://frontend.example"},
	}, false, nil, &syssettings.FakeStore{Enabled: true})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://auth.example.test/api/v1/auth/logout", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("deployed logout status = %d, want %d", recorder.Code, http.StatusOK)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("deployed logout returned %d cookies, want one", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "__Host-lwc_refresh" || cookie.Domain != "" || cookie.Path != "/" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != -1 {
		t.Fatalf("deployed logout cookie attributes: name=%q domain=%q path=%q secure=%v httpOnly=%v sameSite=%v maxAge=%d", cookie.Name, cookie.Domain, cookie.Path, cookie.Secure, cookie.HttpOnly, cookie.SameSite, cookie.MaxAge)
	}
}

func TestAuthHostAllowlistRejectsBeforeRouteHandling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(config.Config{
		DevJWT:         true,
		JWTSecret:      "test-secret",
		AllowedHosts:   []string{"auth.example.test"},
		AllowedOrigins: []string{"https://frontend.example"},
	}, false, nil, &syssettings.FakeStore{Enabled: true})

	recorder := httptest.NewRecorder()
	for _, rawURL := range []string{"http://wrong.example.test/healthz", "http://auth.example.test./healthz"} {
		recorder = httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, rawURL, nil)
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("wrong Host %q status = %d, want %d", rawURL, recorder.Code, http.StatusBadRequest)
		}
		if strings.Contains(recorder.Body.String(), "listening") {
			t.Fatalf("wrong Host %q reached route handling", rawURL)
		}
	}

	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://auth.example.test/healthz", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("allowed Host status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

func TestAuthLocalHostAllowlistAcceptsLocalhostOnlyInLocalMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	localRouter := newProductionRouter(config.Config{DevJWT: true, JWTSecret: "test-secret"}, true, nil, &syssettings.FakeStore{Enabled: true})
	for _, host := range []string{"localhost:8081", "127.0.0.1:8081"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/healthz", nil)
		localRouter.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("local Host %q status = %d, want %d", host, recorder.Code, http.StatusOK)
		}
	}

	productionRouter := newProductionRouter(config.Config{JWTSecret: "test-secret", AllowedOrigins: []string{"https://frontend.example"}}, false, nil, &syssettings.FakeStore{Enabled: true})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://localhost/healthz", nil)
	productionRouter.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("production localhost status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestAuthCORSAllowsOnlyBaselineMethodsAndHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(config.Config{
		DevJWT:         true,
		JWTSecret:      "test-secret",
		AllowedHosts:   []string{"auth.example.test"},
		AllowedOrigins: []string{"https://frontend.example"},
	}, false, nil, &syssettings.FakeStore{Enabled: true})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "http://auth.example.test/api/v1/auth/login", nil)
	request.Header.Set("Origin", "https://frontend.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "Content-Type")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("CORS preflight status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Methods"); got != "GET,POST,OPTIONS" {
		t.Fatalf("Access-Control-Allow-Methods = %q, want %q", got, "GET,POST,OPTIONS")
	}
	if got := recorder.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want %q", got, "Content-Type")
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "https://frontend.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want configured origin", got)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("Access-Control-Allow-Credentials = %q, want true", got)
	}
}

func TestAuthRequestBodyLimitRejectsOversizedLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newProductionRouter(config.Config{DevJWT: true, JWTSecret: "test-secret"}, true, nil, &syssettings.FakeStore{Enabled: true})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://localhost:8081/api/v1/auth/login", strings.NewReader(`{"email":"demo@llm-wiki.dev","password":"`+strings.Repeat("x", 64<<10)+`"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized login status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
