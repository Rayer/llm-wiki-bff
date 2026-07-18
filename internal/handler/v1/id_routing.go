package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

var (
	errIDMapNotFound = errors.New("id map not found")
	idSlugRE         = regexp.MustCompile(`^([a-f0-9]{12})-(.+)$`)
	idOnlyRE         = regexp.MustCompile(`^[a-f0-9]{12}$`)
	wikilinkRE       = regexp.MustCompile(`\[\[([^\[\]\n]+)\]\]`)
)

const idRouteRedirectStatus = http.StatusFound

type idRouteEntry struct {
	ID   string
	Slug string
	Type string
}

type dualIDMap struct {
	byID   map[string]idRouteEntry
	bySlug map[string][]idRouteEntry
}

func loadDualIDMap(ctx context.Context, store idMapStore) (dualIDMap, error) {
	data, err := store.ReadFile(ctx, idMapPath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return dualIDMap{}, errIDMapNotFound
		}
		return dualIDMap{}, fmt.Errorf("read id map: %w", err)
	}

	source, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return dualIDMap{}, fmt.Errorf("decode id map: %w", err)
	}
	return buildDualIDMap(source), nil
}

func buildDualIDMap(source idMap) dualIDMap {
	dual := dualIDMap{
		byID:   map[string]idRouteEntry{},
		bySlug: map[string][]idRouteEntry{},
	}
	addDualIDMapEntries(dual, source.Concept, "concept")
	addDualIDMapEntries(dual, source.Source, "source")
	return dual
}

func addDualIDMapEntries(dual dualIDMap, entries map[string]string, entryType string) {
	for id, slug := range entries {
		id = strings.TrimSpace(id)
		slug = strings.TrimSpace(slug)
		if id == "" || slug == "" {
			continue
		}
		entry := idRouteEntry{ID: id, Slug: slug, Type: entryType}
		dual.byID[id] = entry
		dual.bySlug[strings.ToLower(slug)] = append(dual.bySlug[strings.ToLower(slug)], entry)
	}
}

func rewriteWikilinks(markdownContent string, dual dualIDMap) string {
	return wikilinkRE.ReplaceAllStringFunc(markdownContent, func(link string) string {
		inner := link[2 : len(link)-2]
		target, alias, hasAlias := strings.Cut(inner, "|")
		base, anchor, hasAnchor := strings.Cut(target, "#")
		base = strings.TrimSpace(base)
		if strings.HasPrefix(base, "concepts/") || strings.HasPrefix(base, "sources/") {
			idx := strings.Index(base, "/")
			idSlug := strings.TrimSpace(base[idx+1:])
			if id, _, ok := idFromPathValue(idSlug); ok {
				if entry, exists := dual.byID[id]; exists {
					return canonicalWikilinkTarget(entry, anchor, hasAnchor, alias, hasAlias)
				}
			}
			return link
		}

		entries := dual.bySlug[strings.ToLower(base)]
		if len(entries) == 0 {
			return link
		}
		entry := entries[0]
		return canonicalWikilinkTarget(entry, anchor, hasAnchor, alias, hasAlias)
	})
}

func canonicalWikilinkTarget(entry idRouteEntry, anchor string, hasAnchor bool, alias string, hasAlias bool) string {
	target := routePrefix(entry.Type) + "/" + entryIDSlug(entry)
	if hasAnchor {
		target += "#" + anchor
	}
	label := entry.Slug
	if hasAlias {
		label = alias
	}
	return "[[" + target + "|" + label + "]]"
}

func parseIDSlug(value string) (string, string, bool) {
	matches := idSlugRE.FindStringSubmatch(value)
	if len(matches) != 3 {
		return "", "", false
	}
	return matches[1], matches[2], true
}

func idFromPathValue(value string) (string, string, bool) {
	if id, slug, ok := parseIDSlug(value); ok {
		return id, slug, true
	}
	if idOnlyRE.MatchString(value) {
		return value, "", true
	}
	return "", "", false
}

func canonicalIDRoute(currentType, idSlug string, dual dualIDMap) (string, bool) {
	id, slug, ok := idFromPathValue(idSlug)
	if !ok {
		return "", false
	}
	entry, ok := dual.byID[id]
	if !ok {
		return "", false
	}
	if entry.Type == currentType && slug == entry.Slug {
		return "", false
	}
	return "/" + routePrefix(entry.Type) + "/" + entryIDSlug(entry), true
}

func routePrefix(entryType string) string {
	if entryType == "source" {
		return "sources"
	}
	return "concepts"
}

func entryIDSlug(entry idRouteEntry) string {
	return entry.ID + "-" + entry.Slug
}

func (h *Handler) getIDRoutingMap(ctx context.Context, gcsClient store.Store) (dualIDMap, error) {
	prefix := viewCacheKey(gcsClient)

	h.idRoutingMu.Lock()
	defer h.idRoutingMu.Unlock()
	if h.idRoutingMaps == nil {
		h.idRoutingMaps = map[string]dualIDMap{}
	}
	if dual, ok := h.idRoutingMaps[prefix]; ok {
		return dual, nil
	}

	dual, err := loadDualIDMap(ctx, gcsClient)
	if err != nil {
		return dualIDMap{}, err
	}
	h.idRoutingMaps[prefix] = dual
	return dual, nil
}

func (h *Handler) rewriteMarkdownForResponse(c *gin.Context, gcsClient store.Store, data []byte) ([]byte, bool) {
	dual, err := h.getIDRoutingMap(c.Request.Context(), gcsClient)
	if err != nil {
		status := http.StatusInternalServerError
		message := "generated data unavailable"
		if errors.Is(err, errIDMapNotFound) {
			status = http.StatusNotFound
			message = "id map not found"
		}
		c.JSON(status, handler.ErrorResponse{Error: message})
		return nil, false
	}
	return []byte(rewriteWikilinks(string(data), dual)), true
}

func (h *Handler) handleIDRoutedPage(c *gin.Context, gcsClient store.Store, currentType, idSlug string) bool {
	if _, _, ok := idFromPathValue(idSlug); !ok {
		return false
	}

	dual, err := h.getIDRoutingMap(c.Request.Context(), gcsClient)
	if err != nil {
		status := http.StatusInternalServerError
		message := "generated data unavailable"
		if errors.Is(err, errIDMapNotFound) {
			status = http.StatusNotFound
			message = "id map not found"
		}
		c.JSON(status, handler.ErrorResponse{Error: message})
		return true
	}

	id, _, _ := idFromPathValue(idSlug)
	entry, ok := dual.byID[id]
	if !ok {
		c.JSON(http.StatusNotFound, handler.ErrorResponse{Error: "id not found: " + id})
		return true
	}
	if target, ok := canonicalIDRoute(currentType, idSlug, dual); ok {
		c.Redirect(idRouteRedirectStatus, requestRelativeIDRoute(c, target))
		return true
	}

	category := routePrefix(entry.Type)
	page, data, err := gcsClient.GetPage(c.Request.Context(), entry.Slug, category)
	if err != nil {
		writeGeneratedReadError(c, err, entry.Type+" not found: "+entry.Slug)
		return true
	}
	data = []byte(rewriteWikilinks(string(data), dual))
	frontmatter, body := parseFrontmatter(string(data))

	if entry.Type == "source" {
		page.ID = entry.ID
		response, err := h.sourceDetailResponse(c, gcsClient, *page, data)
		if err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: generatedDataUnavailableMessage})
			return true
		}
		c.JSON(http.StatusOK, response)
		return true
	}

	c.JSON(http.StatusOK, handler.ConceptDetailResponse{
		Slug:        entry.Slug,
		Title:       entry.Slug,
		Type:        "concept",
		Status:      page.Status,
		Frontmatter: frontmatter,
		Body:        body,
		Raw:         string(data),
	})
	return true
}

func requestRelativeIDRoute(c *gin.Context, target string) string {
	path := c.Request.URL.Path
	for _, marker := range []string{"/concepts/", "/sources/"} {
		if idx := strings.LastIndex(path, marker); idx >= 0 {
			return path[:idx] + target
		}
	}
	return target
}
