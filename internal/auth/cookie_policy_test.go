package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/firestore"
	"github.com/gin-gonic/gin"
)

func TestRefreshCookiePoliciesSetExactRotationAndLogoutAttributes(t *testing.T) {
	tests := []struct {
		name       string
		policy     RefreshCookiePolicy
		wantName   string
		wantDomain string
		wantSecure bool
	}{
		{name: "BFF legacy", policy: LegacyRefreshCookiePolicy(), wantName: "refresh_token", wantDomain: "rayer.idv.tw", wantSecure: true},
		{name: "standalone host", policy: HostRefreshCookiePolicy(), wantName: "__Host-lwc_refresh", wantDomain: "", wantSecure: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRefreshTokensForTest()
			refreshToken, err := GenerateRefreshToken("user-123", "", "test-secret")
			if err != nil {
				t.Fatalf("generate refresh token: %v", err)
			}
			lookup := func(_ context.Context, _ *firestore.Client, _ string) (*UserRecord, error) {
				return &UserRecord{Email: "demo@example.com"}, nil
			}

			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/refresh", refreshHandlerWithCookiePolicy(nil, "test-secret", lookup, tt.policy))
			request := httptest.NewRequest(http.MethodPost, "/refresh", nil)
			request.AddCookie(&http.Cookie{Name: tt.policy.Name, Value: refreshToken})
			rotation := httptest.NewRecorder()
			router.ServeHTTP(rotation, request)
			if rotation.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want %d", rotation.Code, http.StatusOK)
			}
			rotated := cookieNamed(t, rotation, tt.policy.Name)
			assertPolicyCookie(t, rotated, tt.wantName, tt.wantDomain, tt.wantSecure, int(refreshTokenTTL.Seconds()))

			router = gin.New()
			router.POST("/logout", LogoutHandlerWithCookiePolicy(tt.policy))
			logout := httptest.NewRecorder()
			router.ServeHTTP(logout, httptest.NewRequest(http.MethodPost, "/logout", nil))
			cleared := cookieNamed(t, logout, tt.policy.Name)
			assertPolicyCookie(t, cleared, tt.wantName, tt.wantDomain, tt.wantSecure, -1)
			if cleared.Value != "" {
				t.Fatal("logout cookie was not cleared")
			}
		})
	}
}

func cookieNamed(t *testing.T, recorder *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %q missing", name)
	return nil
}

func assertPolicyCookie(t *testing.T, cookie *http.Cookie, wantName, wantDomain string, wantSecure bool, wantMaxAge int) {
	t.Helper()
	if cookie.Name != wantName || cookie.Domain != wantDomain || cookie.Path != "/" || cookie.Secure != wantSecure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != wantMaxAge {
		t.Fatalf("cookie attributes: name=%q domain=%q path=%q secure=%v httpOnly=%v sameSite=%v maxAge=%d", cookie.Name, cookie.Domain, cookie.Path, cookie.Secure, cookie.HttpOnly, cookie.SameSite, cookie.MaxAge)
	}
}
