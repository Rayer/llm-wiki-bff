package main

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/config"
	handlerv1 "github.com/rayer/llm-wiki-bff/internal/handler/v1"
	"github.com/rayer/llm-wiki-bff/internal/syssettings"
)

func TestProductionRouterDoesNotExposeAuthRoutes(t *testing.T) {
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

	for _, route := range router.Routes() {
		if len(route.Path) >= len("/api/v1/auth/") && route.Path[:len("/api/v1/auth/")] == "/api/v1/auth/" {
			t.Fatalf("BFF production router registered auth route %s %s", route.Method, route.Path)
		}
	}

	for _, path := range []string{"/api/v1/public/config", "/api/v1/public/version"} {
		if !hasRoute(router, http.MethodGet, path) {
			t.Fatalf("BFF production router is missing GET %s", path)
		}
	}
}

func hasRoute(router *gin.Engine, method, path string) bool {
	for _, route := range router.Routes() {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}
