package v1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex/fsstore"
)

func planSyntoGeneration(ctx context.Context, workspace string) (store.GenerationRebuildPlan, error) {
	indexStore := fsstore.New(workspace)
	priorData, priorErr := indexStore.ReadFile(ctx, wikiindex.IDMapPath)
	var prior wikiindex.IDMap
	if priorErr == nil {
		var err error
		prior, err = wikiindex.DecodeIDMap(priorData)
		if err != nil {
			return store.GenerationRebuildPlan{}, errors.New("prior_id_map_invalid")
		}
	} else if !errors.Is(priorErr, wikiindex.ErrNotFound) {
		return store.GenerationRebuildPlan{}, errors.New("prior_id_map_read")
	}
	indexData, err := os.ReadFile(filepath.Join(workspace, ".synto", "INDEX.json"))
	if err != nil {
		return store.GenerationRebuildPlan{}, errors.New("synto_index_read")
	}
	plan, err := wikiindex.DecodeSyntoIdentityPlan(indexData)
	if err != nil {
		return store.GenerationRebuildPlan{}, fmt.Errorf("synto_index_invalid: %w", err)
	}
	next, err := wikiindex.RebuildWithSyntoIdentity(ctx, indexStore, plan)
	if err != nil {
		return store.GenerationRebuildPlan{}, err
	}
	migrated := 0
	for oldID, slug := range prior.Concept {
		if oldID == "" || slug == "" {
			continue
		}
		if _, stillCurrent := next.Concept[oldID]; stillCurrent {
			continue
		}
		entityID := ""
		for candidate, candidateSlug := range next.Concept {
			if candidateSlug != slug {
				continue
			}
			if entityID != "" {
				entityID = ""
				break
			}
			entityID = candidate
		}
		if entityID == "" || entityID == oldID {
			continue
		}
		if next.IDRedirects == nil {
			next.IDRedirects = make(map[string]string)
		}
		next.IDRedirects[oldID] = entityID
		migrated++
	}
	encoded, err := wikiindex.EncodeIDMap(next)
	if err != nil {
		return store.GenerationRebuildPlan{}, errors.New("id_map_encode")
	}
	if _, err := indexStore.WriteBytesAtomic(ctx, encoded, wikiindex.IDMapTempPath, wikiindex.IDMapPath); err != nil {
		return store.GenerationRebuildPlan{}, errors.New("id_map_write")
	}
	redirects := len(next.IDRedirects)
	for _, values := range next.Redirects {
		redirects += len(values)
	}
	return store.GenerationRebuildPlan{
		ConceptCount:   len(next.Concept),
		SourceCount:    len(next.Source),
		MigratedOldIDs: migrated,
		RedirectCount:  redirects,
	}, nil
}
