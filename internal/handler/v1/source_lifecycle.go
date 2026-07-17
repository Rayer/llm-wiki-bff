package v1

import (
	"context"
	"errors"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
	"github.com/rayer/llm-wiki-bff/internal/sourcelifecycle"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

// sourceLifecycle loads the three collection-level artifacts used by all
// lifecycle consumers. Missing or malformed worker receipts intentionally mean
// no receipt, preserving rollout compatibility.
func sourceLifecycle(ctx context.Context, s store.Store, sources []store.WikiPage) ([]store.WikiPage, sourcelifecycle.Counts, error) {
	return sourceLifecycleWithAnnotations(ctx, s, sources, nil)
}

func sourceLifecycleWithAnnotations(ctx context.Context, s store.Store, sources []store.WikiPage, overrides map[string]store.ObjectMeta) ([]store.WikiPage, sourcelifecycle.Counts, error) {
	receipts := map[string]sourcestatus.Receipt{}
	if data, err := s.ReadFile(ctx, sourcestatus.Path); err == nil {
		if artifact, err := sourcestatus.Decode(data); err == nil && artifact.Version == 1 {
			receipts = artifact.Sources
		}
	} else if !errors.Is(err, storage.ErrObjectNotExist) {
		return nil, sourcelifecycle.Counts{}, err
	}
	legacy := map[string]rawstatus.FileStatus{}
	var legacyGeneratedAt time.Time
	if data, err := s.ReadFile(ctx, rawstatus.Path); err == nil {
		if artifact, err := rawstatus.Decode(data); err == nil && artifact.Version == 1 {
			legacy = artifact.Files
			legacyGeneratedAt, _ = time.Parse(time.RFC3339, artifact.GeneratedAt)
		}
	} else if !errors.Is(err, storage.ErrObjectNotExist) {
		return nil, sourcelifecycle.Counts{}, err
	}
	annotations := map[string]store.ObjectMeta{}
	if lister, ok := s.(store.ObjectLister); ok {
		entries, err := lister.ListObjectMeta(ctx, "cache/annotations/")
		if err != nil {
			return nil, sourcelifecycle.Counts{}, err
		}
		for _, entry := range entries {
			id := strings.TrimSuffix(strings.TrimPrefix(entry.Path, "cache/annotations/"), ".json")
			if id != "" {
				annotations[id] = entry
			}
		}
	}
	for id, meta := range overrides {
		annotations[id] = meta
	}
	rawFiles, err := s.ListRawFiles(ctx)
	if err != nil {
		return nil, sourcelifecycle.Counts{}, err
	}
	pages, counts := sourcelifecycle.Calculate(sourcelifecycle.Input{Sources: sources, RawFiles: rawFiles, Annotations: annotations, Receipts: receipts, Legacy: legacy, LegacyGeneratedAt: legacyGeneratedAt})
	return pages, counts, nil
}

func sourceByID(pages []store.WikiPage, id string) (store.WikiPage, bool) {
	for _, page := range pages {
		if page.ID == id {
			return page, true
		}
	}
	return store.WikiPage{}, false
}

func (h *Handler) sourceDetailResponse(c *gin.Context, s store.Store, page store.WikiPage, data []byte) (handler.SourceDetailResponse, error) {
	frontmatter, body := parseFrontmatter(string(data))
	if page.ID == "" {
		page.ID, _ = frontmatter["id"].(string)
	}
	if page.RawPath == "" {
		page.RawPath, _ = frontmatter["source_file"].(string)
	}
	pages, _, err := sourceLifecycle(c.Request.Context(), s, []store.WikiPage{page})
	if err != nil {
		return handler.SourceDetailResponse{}, err
	}
	if page, ok := sourceByID(pages, page.ID); ok {
		return handler.SourceDetailResponse{ID: page.ID, Slug: page.Slug, Title: page.Title, Type: "source", Frontmatter: frontmatter, Body: body, Raw: string(data), RawPath: page.RawPath, AnnotationAllowed: page.AnnotationAllowed, HasAnnotation: page.HasAnnotation, AnnotationDirty: page.AnnotationDirty, RawDirty: page.RawDirty, Dirty: page.Dirty, LifecycleStatus: page.LifecycleStatus, AnnUpdatedAt: page.AnnUpdatedAt}, nil
	}
	return handler.SourceDetailResponse{}, errors.New("source missing from lifecycle response")
}
