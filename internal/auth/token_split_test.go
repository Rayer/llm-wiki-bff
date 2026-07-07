package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func TestGenerateAccessTokenExpiresIn15Minutes(t *testing.T) {
	tokenString, err := GenerateAccessToken("user-123", "test-secret")
	if err != nil {
		t.Fatalf("generate access token: %v", err)
	}

	claims := parseTestClaims(t, tokenString, "test-secret")
	assertExpiryNear(t, claims.ExpiresAt.Time, 15*time.Minute)
}

func TestGenerateRefreshTokenExpiresIn7Days(t *testing.T) {
	resetRefreshTokensForTest()
	tokenString, err := GenerateRefreshToken("user-123", "test-secret")
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}

	claims := parseTestClaims(t, tokenString, "test-secret")
	assertExpiryNear(t, claims.ExpiresAt.Time, 7*24*time.Hour)
}

func TestRefreshHandlerRotatesRefreshToken(t *testing.T) {
	resetRefreshTokensForTest()
	oldToken, err := GenerateRefreshToken("user-123", "test-secret")
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}
	getUser := func(_ context.Context, _ *firestore.Client, userID string) (*UserRecord, error) {
		if userID != "user-123" {
			t.Fatalf("userID = %q, want %q", userID, "user-123")
		}
		return &UserRecord{Email: "demo@example.com"}, nil
	}

	first := performRefreshRequest(oldToken, getUser)
	if first.Code != http.StatusOK {
		t.Fatalf("first refresh status = %d, body = %s", first.Code, first.Body.String())
	}
	var body struct {
		AccessToken string `json:"access_token"`
		User        User   `json:"user"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refresh response: %v", err)
	}
	if body.AccessToken == "" {
		t.Fatalf("access_token missing from response: %s", first.Body.String())
	}
	if body.User.ID != "user-123" || body.User.Email != "demo@example.com" {
		t.Fatalf("user = %+v, want id and email", body.User)
	}
	newCookie := findRefreshCookie(t, first)
	assertRefreshCookieAttrs(t, newCookie, int(refreshTokenTTL.Seconds()))
	if newCookie.Value == "" || newCookie.Value == oldToken {
		t.Fatalf("refresh token was not rotated")
	}

	second := performRefreshRequest(oldToken, getUser)
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("old refresh token status = %d, want %d", second.Code, http.StatusUnauthorized)
	}
}

func TestLogoutClearsRefreshCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/auth/logout", LogoutHandler())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	cookie := findRefreshCookie(t, rec)
	if cookie.MaxAge != -1 && cookie.MaxAge != 0 {
		t.Fatalf("MaxAge = %d, want cleared cookie", cookie.MaxAge)
	}
	if cookie.Value != "" {
		t.Fatalf("cookie value = %q, want empty", cookie.Value)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attrs = HttpOnly:%v Secure:%v SameSite:%v, want httpOnly secure lax", cookie.HttpOnly, cookie.Secure, cookie.SameSite)
	}
}

func TestRefreshHandlerRejectsInvalidRefreshToken(t *testing.T) {
	resetRefreshTokensForTest()
	rec := performRefreshRequest("not-a-jwt", nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func performRefreshRequest(refreshToken string, lookup userLookupFunc) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if lookup == nil {
		lookup = GetUser
	}
	router.POST("/api/v1/auth/refresh", refreshHandler(nil, "test-secret", lookup))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshTokenCookieName, Value: refreshToken})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func parseTestClaims(t *testing.T, tokenString, secret string) *Claims {
	t.Helper()
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if !token.Valid {
		t.Fatalf("token is invalid")
	}
	return claims
}

func assertExpiryNear(t *testing.T, got time.Time, wantTTL time.Duration) {
	t.Helper()
	ttl := time.Until(got)
	if ttl < wantTTL-2*time.Second || ttl > wantTTL+2*time.Second {
		t.Fatalf("ttl = %s, want near %s", ttl, wantTTL)
	}
}

func findRefreshCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == refreshTokenCookieName {
			return cookie
		}
	}
	t.Fatalf("refresh cookie missing; Set-Cookie = %v", rec.Header().Values("Set-Cookie"))
	return nil
}

func assertRefreshCookieAttrs(t *testing.T, cookie *http.Cookie, wantMaxAge int) {
	t.Helper()
	if cookie.Path != refreshTokenCookiePath {
		t.Fatalf("cookie Path = %q, want %q", cookie.Path, refreshTokenCookiePath)
	}
	if cookie.Domain != refreshTokenDomain {
		t.Fatalf("cookie Domain = %q, want %q", cookie.Domain, refreshTokenDomain)
	}
	if cookie.MaxAge != wantMaxAge {
		t.Fatalf("cookie MaxAge = %d, want %d", cookie.MaxAge, wantMaxAge)
	}
	if !cookie.HttpOnly {
		t.Fatalf("cookie HttpOnly = false, want true")
	}
	if !cookie.Secure {
		t.Fatalf("cookie Secure = false, want true")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie SameSite = %v, want %v", cookie.SameSite, http.SameSiteLaxMode)
	}
}
