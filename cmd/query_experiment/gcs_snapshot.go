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
	"sync"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/generation"
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
	if !validURIComponent(u.Host) {
		return gcsProjectRoot{}, errors.New("snapshot must be a canonical gs:// Project-root URI")
	}
	return gcsProjectRoot{bucket: u.Host, userID: parts[1], project: parts[3]}, nil
}

func resolveSnapshotLocator(options experimentOptions) (string, error) {
	snapshot := strings.TrimSpace(options.snapshotPath)
	bucket := strings.TrimSpace(options.gcsBucket)
	userID := strings.TrimSpace(options.gcsUserID)
	projectID := strings.TrimSpace(options.projectID)
	if snapshot != "" && (bucket != "" || userID != "" || projectID != "") {
		return "", errors.New("snapshot and split GCS flags are mutually exclusive")
	}
	if snapshot == "" && bucket == "" && userID == "" && projectID == "" {
		return "", errors.New("snapshot or all split GCS flags are required")
	}
	if snapshot == "" {
		if !validURIComponent(bucket) || !validURIComponent(userID) || !validURIComponent(projectID) {
			return "", errors.New("gcs bucket, user ID, and project ID must be non-empty safe components")
		}
		return "gs://" + bucket + "/users/" + userID + "/projects/" + projectID, nil
	}
	return snapshot, nil
}

func validURIComponent(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, `%/\\`) && path.Clean(value) == value
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
	var closeOnce sync.Once
	var closeErr error
	cleanup := func() error {
		closeOnce.Do(func() { closeErr = client.Close() })
		return closeErr
	}
	pinned, snapshot, err := client.WithScope(parsed.userID, parsed.project).PinCurrentGeneration(ctx)
	if err != nil {
		_ = cleanup()
		return preparedSnapshot{}, err
	}
	prepared := prepareRemoteSnapshot(pinned, snapshot, sanitizedProjectRoot())
	prepared.cleanup = cleanup
	prepared, err = readRemoteArtifacts(ctx, pinned, prepared)
	if err != nil {
		_ = cleanup()
		return preparedSnapshot{}, err
	}
	prepared.cache = cache.New()
	if _, err := prepared.cache.All(ctx, prepared.reader); err != nil {
		_ = cleanup()
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
	concepts, err := readRemoteArtifact(ctx, reader, conceptsPath, generation.MaxFileBytes)
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot corpus: %w", err)
	}
	suggested, err := readRemoteArtifact(ctx, reader, suggestedPath, suggestedqueries.MaxArtifactBytes)
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot suggested queries: %w", err)
	}
	artifact, err := suggestedqueries.Decode(suggested)
	if err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot suggested queries: %w", err)
	}
	if err := suggestedqueries.ValidatePublishedArtifact(artifact); err != nil {
		return preparedSnapshot{}, fmt.Errorf("snapshot suggested queries: %w", err)
	}
	digest := sha256.Sum256(concepts)
	prepared := snapshot
	prepared.digest = hex.EncodeToString(digest[:])
	suggestedDigest := sha256.Sum256(suggested)
	prepared.suggestedDigest = hex.EncodeToString(suggestedDigest[:])
	prepared.suggestedData = suggested
	prepared.suggestedDataSet = true
	prepared.reader = &frozenSnapshotReader{Reader: reader, concepts: concepts, suggested: suggested}
	return prepared, nil
}

func readRemoteArtifact(ctx context.Context, reader gcsSnapshotSource, relPath string, limit int) ([]byte, error) {
	var (
		data []byte
		err  error
	)
	if bounded, ok := reader.(interface {
		ReadFileLimited(context.Context, string, int64) ([]byte, error)
	}); ok {
		data, err = bounded.ReadFileLimited(ctx, relPath, int64(limit))
	} else {
		data, err = reader.ReadFile(ctx, relPath)
	}
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("snapshot artifact %s exceeds %d-byte limit", relPath, limit)
	}
	return data, nil
}

type frozenSnapshotReader struct {
	cache.Reader
	concepts  []byte
	suggested []byte
}

func (r *frozenSnapshotReader) ViewToken() string {
	if tokenized, ok := r.Reader.(interface{ ViewToken() string }); ok {
		return tokenized.ViewToken()
	}
	return ""
}

func (r *frozenSnapshotReader) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch relPath {
	case conceptsPath:
		return r.concepts, nil
	case suggestedPath:
		return r.suggested, nil
	default:
		return r.Reader.(interface {
			ReadFile(context.Context, string) ([]byte, error)
		}).ReadFile(ctx, relPath)
	}
}

func (r *frozenSnapshotReader) ReadFileLimited(ctx context.Context, relPath string, limit int64) ([]byte, error) {
	if data, err := r.ReadFile(ctx, relPath); err == nil {
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("snapshot artifact %s exceeds %d-byte limit", relPath, limit)
		}
		return data, nil
	} else if bounded, ok := r.Reader.(interface {
		ReadFileLimited(context.Context, string, int64) ([]byte, error)
	}); ok {
		return bounded.ReadFileLimited(ctx, relPath, limit)
	} else {
		return nil, err
	}
}
