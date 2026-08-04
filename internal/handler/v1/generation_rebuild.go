package v1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rayer/llm-wiki-bff/internal/generation"
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
	indexData, err := readBoundedSyntoIndex(workspace)
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
		if target, ok := next.IDRedirects[oldID]; ok && target != oldID && slug == next.Concept[target] && wikiindex.ValidLegacyConceptID(oldID) {
			if prior.IDRedirects[oldID] != target {
				migrated++
			}
		}
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

func readBoundedSyntoIndex(workspace string) ([]byte, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	const rel = ".synto/INDEX.json"
	info, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > generation.MaxFileBytes {
		return nil, errors.New("Synto INDEX exceeds generation size limit")
	}
	file, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() || !os.SameFile(info, openedInfo) {
		return nil, errors.New("Synto INDEX changed while reading")
	}
	data, err := io.ReadAll(io.LimitReader(file, generation.MaxFileBytes+1))
	if err != nil || int64(len(data)) != info.Size() || int64(len(data)) > generation.MaxFileBytes {
		return nil, errors.New("Synto INDEX exceeds generation size limit")
	}
	return data, nil
}
