package suggestedqueries

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/generation"
)

const (
	Path       = "cache/suggested_queries.json"
	MaxQueries = 5
)

type Artifact struct {
	Version    int         `json:"version"`
	Queries    []string    `json:"queries"`
	Candidates []Candidate `json:"candidates"`
	UpdatedAt  string      `json:"updated_at"`
}

func Decode(data []byte) (Artifact, error) {
	var artifact Artifact
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return Artifact{}, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return Artifact{}, fmt.Errorf("expected JSON object")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return Artifact{}, err
		}
		name, ok := key.(string)
		if !ok {
			return Artifact{}, fmt.Errorf("expected JSON object key")
		}
		switch name {
		case "version":
			err = dec.Decode(&artifact.Version)
		case "queries":
			artifact.Queries, err = generation.DecodeBoundedStrings(dec)
		case "candidates":
			artifact.Candidates, err = decodeCandidates(dec)
		case "updated_at":
			err = dec.Decode(&artifact.UpdatedAt)
		default:
			var ignored json.RawMessage
			err = dec.Decode(&ignored)
		}
		if err != nil {
			return Artifact{}, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return Artifact{}, err
	}
	if err := generation.EnsureJSONEOF(dec); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}

func decodeCandidates(dec *json.Decoder) ([]Candidate, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("expected candidates array")
	}
	candidates := make([]Candidate, 0, MinQueries)
	for dec.More() {
		if len(candidates) >= MaxQueries {
			return nil, generation.ErrLogicalEntryLimit
		}
		var candidate Candidate
		if err := dec.Decode(&candidate); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if token, err := dec.Token(); err != nil || token != json.Delim(']') {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("expected candidates array end")
	}
	return candidates, nil
}

func Queries(artifact Artifact) []string {
	if len(artifact.Queries) == 0 {
		return []string{}
	}
	return append([]string(nil), artifact.Queries...)
}

func conceptUpdatedAt(entry conceptcache.Entry, mtimes map[string]time.Time, order int) time.Time {
	if updated := frontmatterTime(entry.Frontmatter["updated"]); !updated.IsZero() {
		return updated
	}
	if mtimes != nil {
		if mtime, ok := mtimes[entry.Slug]; ok {
			return mtime.UTC()
		}
	}
	return time.Unix(0, int64(order))
}

func frontmatterTime(value interface{}) time.Time {
	text, ok := value.(string)
	if !ok {
		return time.Time{}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
