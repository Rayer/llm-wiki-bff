package suggestedqueries

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
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
	Queries   []string `json:"queries"`
	UpdatedAt string   `json:"updated_at"`
}

type rankedConcept struct {
	title string
	when  time.Time
	order int
}

func Build(entries []conceptcache.Entry, mtimes map[string]time.Time, now time.Time) Artifact {
	ranked := make([]rankedConcept, 0, len(entries))
	for i, entry := range entries {
		title := strings.TrimSpace(entry.Title)
		if title == "" {
			continue
		}
		ranked = append(ranked, rankedConcept{
			title: title,
			when:  conceptUpdatedAt(entry, mtimes, i),
			order: i,
		})
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].when.Equal(ranked[j].when) {
			return ranked[i].order > ranked[j].order
		}
		return ranked[i].when.After(ranked[j].when)
	})

	limit := len(ranked)
	if limit > MaxQueries {
		limit = MaxQueries
	}
	queries := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		queries = append(queries, ranked[i].title)
	}

	return Artifact{
		Queries:   queries,
		UpdatedAt: now.UTC().Format(time.RFC3339),
	}
}

func BuildFromConceptsJSONL(data []byte, mtimes map[string]time.Time, now time.Time) (Artifact, error) {
	entries, err := parseConceptsJSONL(data)
	if err != nil {
		return Artifact{}, err
	}
	return Build(entries, mtimes, now), nil
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
		case "queries":
			artifact.Queries, err = generation.DecodeBoundedStrings(dec)
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

func Queries(artifact Artifact) []string {
	if len(artifact.Queries) == 0 {
		return []string{}
	}
	return append([]string(nil), artifact.Queries...)
}

func parseConceptsJSONL(data []byte) ([]conceptcache.Entry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	entries := make([]conceptcache.Entry, 0)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(entries) >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		var entry conceptcache.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("decode concepts jsonl line: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
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
