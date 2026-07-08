package main

import (
	"os"
	"strings"
	"testing"
)

func TestStaticIndexHasMobileResponsiveShell(t *testing.T) {
	raw, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	html := string(raw)

	wantSnippets := map[string]string{
		"mobile header":         `class="mobile-header"`,
		"hamburger button":      `id="mobile-menu-toggle"`,
		"drawer aria control":   `aria-controls="sidebar-shell"`,
		"drawer overlay":        `class="drawer-scrim"`,
		"semantic navigation":   `<nav class="sidebar-nav" id="sidebar" aria-label="Wiki navigation">`,
		"semantic article":      `<article id="wiki-view"`,
		"drawer open function":  `function openMobileNav()`,
		"drawer close function": `function closeMobileNav()`,
		"escape closes drawer":  `event.key === 'Escape'`,
		"mobile touch target":   `min-height: 44px`,
		"mobile query wrapping": `grid-template-columns: 1fr auto`,
		"api base":              `const API_BASE = '/api/v1';`,
		"local user header":     `'X-User-ID': 'local-user'`,
		"local project header":  `'X-Project-ID': 'demo'`,
		"query uses v1 POST":    `fetch(API_BASE + '/query', {`,
	}

	for name, snippet := range wantSnippets {
		if !strings.Contains(html, snippet) {
			t.Fatalf("%s: static/index.html missing %q", name, snippet)
		}
	}

	if strings.Contains(html, `class="entry${i === 0 ? ' active' : ''}"`) {
		t.Fatal("sidebar should not mark the first source active before a page is selected")
	}
}
