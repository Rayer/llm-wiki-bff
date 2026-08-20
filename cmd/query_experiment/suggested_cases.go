package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
)

func suggestedCases(data []byte, mode string, existing []caseInput) ([]caseInput, error) {
	if mode != "wiki" && mode != "full" {
		return nil, fmt.Errorf("unsupported suggested-query-mode %q", mode)
	}
	artifact, err := suggestedqueries.Decode(data)
	if err != nil || artifact.Version != 2 {
		if err == nil {
			err = errors.New("unsupported suggested-query schema")
		}
		return nil, fmt.Errorf("suggested queries: %w", err)
	}
	if err := suggestedqueries.ValidatePublishedArtifact(artifact); err != nil {
		return nil, fmt.Errorf("suggested queries: %w", err)
	}
	if len(existing)+len(artifact.Candidates) > maxExperimentCases {
		return nil, fmt.Errorf("cases exceed %d-case limit", maxExperimentCases)
	}
	seen := make(map[string]struct{}, len(existing)+len(artifact.Candidates))
	for _, input := range existing {
		if _, ok := seen[input.ID]; ok {
			return nil, fmt.Errorf("duplicate id %q", input.ID)
		}
		seen[input.ID] = struct{}{}
	}
	result := make([]caseInput, 0, len(artifact.Candidates))
	for i, candidate := range artifact.Candidates {
		id := fmt.Sprintf("suggested-%s-%02d", mode, i+1)
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate generated case id %q", id)
		}
		tags := []string{"suggested-query", "suggested-query-mode:" + mode}
		if strings.TrimSpace(candidate.Intent) != "" {
			tags = append(tags, "intent:"+candidate.Intent)
		}
		for _, anchor := range candidate.CorpusAnchorConceptIDs {
			tags = append(tags, "corpus-anchor:"+anchor)
		}
		result = append(result, caseInput{ID: id, Query: candidate.Question, Mode: mode, Tags: tags})
		seen[id] = struct{}{}
	}
	return result, nil
}
