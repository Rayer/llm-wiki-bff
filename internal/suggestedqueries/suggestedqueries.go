package suggestedqueries

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/generation"
)

const (
	Path       = "cache/suggested_queries.json"
	MaxQueries = 20
	// MaxArtifactBytes bounds status reads and decoding for this small published
	// artifact. It covers a valid current artifact with candidates, anchors, and
	// generation metadata while remaining far below the general generation-file limit.
	MaxArtifactBytes = 128 << 10
)

var (
	ErrArtifactTooLarge     = errors.New("suggested-query artifact exceeds byte limit")
	ErrPublishedQueryTooBig = errors.New("suggested-query query exceeds byte limit")
)

type Artifact struct {
	Version    int         `json:"version"`
	Queries    []string    `json:"queries"`
	Candidates []Candidate `json:"candidates"`
	UpdatedAt  string      `json:"updated_at"`
}

func Decode(data []byte) (Artifact, error) {
	if len(data) > MaxArtifactBytes {
		return Artifact{}, ErrArtifactTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return Artifact{}, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return Artifact{}, fmt.Errorf("expected JSON object")
	}
	var artifact Artifact
	seen := make(map[string]struct{}, 4)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return Artifact{}, err
		}
		name, ok := key.(string)
		if !ok {
			return Artifact{}, fmt.Errorf("expected JSON object key")
		}
		if _, duplicate := seen[name]; duplicate {
			return Artifact{}, fmt.Errorf("duplicate artifact key %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "version":
			artifact.Version, err = decodeJSONInt(dec)
		case "queries":
			artifact.Queries, err = decodePublishedQueries(dec)
		case "candidates":
			artifact.Candidates, err = decodeCandidates(dec)
		case "updated_at":
			artifact.UpdatedAt, err = decodeJSONString(dec)
		default:
			return Artifact{}, fmt.Errorf("unknown artifact key %q", name)
		}
		if err != nil {
			return Artifact{}, err
		}
	}
	if end, err := dec.Token(); err != nil {
		return Artifact{}, err
	} else if end != json.Delim('}') {
		return Artifact{}, fmt.Errorf("expected JSON object end")
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
		candidate, err := decodeCandidate(dec)
		if err != nil {
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

func decodeCandidate(dec *json.Decoder) (Candidate, error) {
	token, err := dec.Token()
	if err != nil {
		return Candidate{}, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return Candidate{}, fmt.Errorf("expected candidate object")
	}
	candidate := Candidate{}
	seen := make(map[string]struct{}, 4)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return Candidate{}, err
		}
		name, ok := key.(string)
		if !ok {
			return Candidate{}, fmt.Errorf("expected candidate object key")
		}
		if _, duplicate := seen[name]; duplicate {
			return Candidate{}, fmt.Errorf("duplicate candidate key %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "question":
			candidate.Question, err = decodeJSONString(dec)
		case "intent/use_case":
			candidate.Intent, err = decodeJSONString(dec)
		case "corpus_anchor_concept_ids":
			candidate.CorpusAnchorConceptIDs, err = decodePublishedAnchorIDs(dec)
		case "generation":
			candidate.Generation, err = decodeGenerationMetadata(dec)
		default:
			return Candidate{}, fmt.Errorf("unknown candidate key %q", name)
		}
		if err != nil {
			return Candidate{}, err
		}
	}
	if end, err := dec.Token(); err != nil {
		return Candidate{}, err
	} else if end != json.Delim('}') {
		return Candidate{}, fmt.Errorf("expected candidate object end")
	}
	return candidate, nil
}

func decodePublishedAnchorIDs(dec *json.Decoder) ([]string, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("expected corpus anchor array")
	}
	ids := make([]string, 0, 1)
	for dec.More() {
		if len(ids) >= MaxConcepts {
			return nil, generation.ErrLogicalEntryLimit
		}
		id, err := decodeJSONString(dec)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if end, err := dec.Token(); err != nil {
		return nil, err
	} else if end != json.Delim(']') {
		return nil, fmt.Errorf("expected corpus anchor array end")
	}
	return ids, nil
}

func decodeGenerationMetadata(dec *json.Decoder) (GenerationMetadata, error) {
	token, err := dec.Token()
	if err != nil {
		return GenerationMetadata{}, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return GenerationMetadata{}, fmt.Errorf("expected generation object")
	}
	metadata := GenerationMetadata{}
	seen := make(map[string]struct{}, 2)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return GenerationMetadata{}, err
		}
		name, ok := key.(string)
		if !ok {
			return GenerationMetadata{}, fmt.Errorf("expected generation object key")
		}
		if _, duplicate := seen[name]; duplicate {
			return GenerationMetadata{}, fmt.Errorf("duplicate generation metadata key %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "model":
			metadata.Model, err = decodeJSONString(dec)
		case "prompt_version":
			metadata.PromptVersion, err = decodeJSONString(dec)
		default:
			return GenerationMetadata{}, fmt.Errorf("unknown generation metadata key %q", name)
		}
		if err != nil {
			return GenerationMetadata{}, err
		}
	}
	if end, err := dec.Token(); err != nil {
		return GenerationMetadata{}, err
	} else if end != json.Delim('}') {
		return GenerationMetadata{}, fmt.Errorf("expected generation object end")
	}
	return metadata, nil
}

func decodePublishedQueries(dec *json.Decoder) ([]string, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("expected queries array")
	}
	queries := make([]string, 0)
	for dec.More() {
		if len(queries) >= MaxQueries {
			return nil, generation.ErrLogicalEntryLimit
		}
		query, err := decodeJSONString(dec)
		if err != nil {
			return nil, err
		}
		if len([]byte(query)) > MaxQuestionBytes {
			return nil, ErrPublishedQueryTooBig
		}
		queries = append(queries, query)
	}
	if end, err := dec.Token(); err != nil {
		return nil, err
	} else if end != json.Delim(']') {
		return nil, fmt.Errorf("expected queries array end")
	}
	return queries, nil
}

func decodeJSONString(dec *json.Decoder) (string, error) {
	token, err := dec.Token()
	if err != nil {
		return "", err
	}
	value, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("expected JSON string")
	}
	return value, nil
}

func decodeJSONInt(dec *json.Decoder) (int, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return 0, err
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return 0, fmt.Errorf("expected JSON number")
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, err
	}
	return value, nil
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
