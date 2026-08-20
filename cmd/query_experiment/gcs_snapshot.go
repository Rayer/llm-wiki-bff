package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
)

const (
	conceptsPath  = "cache/concepts.jsonl"
	suggestedPath = suggestedqueries.Path
)

type gcsProjectRoot struct {
	bucket  string
	userID  string
	project string
}

type gcsSnapshotSource interface {
	cache.Reader
	ReadFile(context.Context, string) ([]byte, error)
}

func parseGCSProjectRoot(raw string) (gcsProjectRoot, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "gs" || u.Opaque != "" || u.Host == "" || u.Port() != "" || u.RawPath != "" || strings.Contains(raw, "%") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return gcsProjectRoot{}, errors.New("snapshot must be a canonical gs:// Project-root URI")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "users" || parts[2] != "projects" || !validURIComponent(parts[1]) || !validURIComponent(parts[3]) {
		return gcsProjectRoot{}, errors.New("snapshot must be gs://<bucket>/users/<user-id>/projects/<project-id>")
	}
	return gcsProjectRoot{bucket: u.Host, userID: parts[1], project: parts[3]}, nil
}

func validURIComponent(value string) bool {
	return value != "" && value != "." && value != ".." && path.Clean(value) == value && !strings.ContainsAny(value, `/\\`)
}

func loadGCSSnapshot(ctx context.Context, root string, newClient func(string) (*gcs.Client, error)) (preparedSnapshot, error) {
	parsed, err := parseGCSProjectRoot(root)
	if err != nil {
		return preparedSnapshot{}, err
	}
	if newClient == nil {
		newClient = gcs.NewClient
	}
	client, err := newClient(parsed.bucket)
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("create GCS client: %w", err)
	}
	defer client.Close()
	pinned, snapshot, err := client.WithScope(parsed.userID, parsed.project).PinCurrentGeneration(ctx)
	if err != nil {
		return preparedSnapshot{}, err
	}
	prepared := prepareRemoteSnapshot(pinned, snapshot, sanitizedProjectRoot())
	prepared, err = readRemoteArtifacts(ctx, pinned, prepared)
	if err != nil {
		return preparedSnapshot{}, err
	}
	prepared.cache = cache.New()
	if _, err := prepared.cache.All(ctx, pinned); err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot corpus: %w", err)
	}
	return prepared, nil
}

func sanitizedProjectRoot() string {
	return "gs://<bucket>/users/<user-id>/projects/<project-id>"
}

func prepareRemoteSnapshot(reader gcsSnapshotSource, snapshot gcs.GenerationSnapshot, label string) preparedSnapshot {
	return preparedSnapshot{
		label:              label,
		reader:             reader,
		manifestGeneration: snapshot.ManifestGeneration,
		manifestDigest:     snapshot.ManifestSHA256,
		generationID:       snapshot.Manifest.GenerationID,
		inputFingerprint:   snapshot.Manifest.InputFingerprint,
	}
}

func readRemoteArtifacts(ctx context.Context, reader gcsSnapshotSource, snapshot preparedSnapshot) (preparedSnapshot, error) {
	concepts, err := reader.ReadFile(ctx, conceptsPath)
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot corpus: %w", err)
	}
	suggested, err := reader.ReadFile(ctx, suggestedPath)
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot suggested queries: %w", err)
	}
	digest := sha256.Sum256(concepts)
	prepared := snapshot
	prepared.digest = hex.EncodeToString(digest[:])
	suggestedDigest := sha256.Sum256(suggested)
	prepared.suggestedDigest = hex.EncodeToString(suggestedDigest[:])
	prepared.suggestedData = suggested
	return prepared, nil
}
