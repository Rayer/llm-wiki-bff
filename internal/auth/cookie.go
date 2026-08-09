package auth

import "net/http"

// RefreshCookiePolicy describes the runtime-specific refresh cookie contract.
type RefreshCookiePolicy struct {
	Name     string
	Domain   string
	Path     string
	Secure   bool
	HttpOnly bool
	SameSite http.SameSite
}

// LegacyRefreshCookiePolicy is the BFF compatibility-lane policy.
func LegacyRefreshCookiePolicy() RefreshCookiePolicy {
	return RefreshCookiePolicy{
		Name:     refreshTokenCookieName,
		Domain:   refreshTokenDomain,
		Path:     refreshTokenCookiePath,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// HostRefreshCookiePolicy is the deployed standalone Auth policy.
func HostRefreshCookiePolicy() RefreshCookiePolicy {
	return RefreshCookiePolicy{
		Name:     "__Host-lwc_refresh",
		Path:     refreshTokenCookiePath,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// LocalRefreshCookiePolicy keeps local HTTP development usable.
func LocalRefreshCookiePolicy() RefreshCookiePolicy {
	return RefreshCookiePolicy{
		Name:     refreshTokenCookieName,
		Path:     refreshTokenCookiePath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}
