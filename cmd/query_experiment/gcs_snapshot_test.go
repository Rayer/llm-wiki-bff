package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
)

func TestParseGCSProjectRootIsStrict(t *testing.T) {
	valid, err := parseGCSProjectRoot("gs://bucket/users/user-1/projects/project-1")
	if err != nil || valid.bucket != "bucket" || valid.userID != "user-1" || valid.project != "project-1" {
		t.Fatalf("valid URI = %#v, %v", valid, err)
	}
	for _, raw := range []string{
		"gs://bucket/users/user/projects/project/",
		"gs://bucket/users/user/projects",
		"gs://bucket/users/../projects/project",
		"https://bucket/users/user/projects/project",
		"gs://bucket/users/user/projects/project?token=secret",
		"gs://bucket:443/users/user/projects/project",
		"gs:bucket/users/user/projects/project",
		"gs://bucket/users/user%2Fpart/projects/project",
		"gs://bucket/users/user/projects/pro%2Fject",
		"gs://bucket/users/user/../projects/project",
	} {
		if _, err := parseGCSProjectRoot(raw); err == nil {
			t.Fatalf("parseGCSProjectRoot(%q) accepted malformed URI", raw)
		}
	}
}

func TestResolveSnapshotLocatorSplitForm(t *testing.T) {
	got, err := resolveSnapshotLocator(experimentOptions{gcsBucket: "bucket", gcsUserID: "user-1", projectID: "project-1"})
	if err != nil || got != "gs://bucket/users/user-1/projects/project-1" {
		t.Fatalf("root = %q, err = %v", got, err)
	}
}

func TestResolveSnapshotLocatorRejectsMissingConflictAndUnsafeValues(t *testing.T) {
	tests := []experimentOptions{
		{gcsUserID: "user", projectID: "project"},
		{gcsBucket: "bucket", projectID: "project"},
		{gcsBucket: "bucket", gcsUserID: "user"},
		{snapshotPath: "./snapshot", gcsBucket: "bucket", gcsUserID: "user", projectID: "project"},
		{gcsBucket: "bucket/../other", gcsUserID: "user", projectID: "project"},
		{gcsBucket: "bucket", gcsUserID: "user%2Fpart", projectID: "project"},
		{gcsBucket: "bucket", gcsUserID: "user", projectID: "../project"},
	}
	for _, options := range tests {
		if _, err := resolveSnapshotLocator(options); err == nil {
			t.Fatalf("accepted invalid locator: %#v", options)
		}
	}
}

func TestInvalidSplitLocatorHasZeroEffects(t *testing.T) {
	called := false
	err := runExperiment(context.Background(), experimentOptions{gcsBucket: "bucket", gcsUserID: "user"}, dependencies{
		loadConfig:   func(string) (config.Config, error) { called = true; return config.Config{}, nil },
		newGCSClient: func(string) (*gcs.Client, error) { called = true; return nil, nil },
		stdout:       &bytes.Buffer{},
	})
	if err == nil || called {
		t.Fatalf("err=%v effects=%v", err, called)
	}
}

func TestSuggestedCasesAreModeSpecificDeterministicAndBounded(t *testing.T) {
	data := validExperimentSuggestedQueries(t)
	one, err := suggestedCases(data, "wiki", nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := suggestedCases(data, "full", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 20 || one[0].ID != "suggested-wiki-01" || one[0].Mode != "wiki" || one[0].Tags[2] != "intent:learn" {
		t.Fatalf("wiki cases = %#v", one)
	}
	if two[0].ID != "suggested-full-01" || two[0].Query != one[0].Query || two[0].Mode != "full" {
		t.Fatalf("full cases = %#v", two)
	}
	if _, err := suggestedCases(data, "bad", nil); err == nil {
		t.Fatal("unsupported mode accepted")
	}
	if _, err := suggestedCases(data, "wiki", []caseInput{{ID: "suggested-wiki-01"}}); err == nil {
		t.Fatal("duplicate generated ID accepted")
	}
}

func validExperimentSuggestedQueries(t *testing.T) []byte {
	t.Helper()
	candidates := make([]suggestedqueries.Candidate, 0, suggestedqueries.RequiredQueries)
	for i := 1; i <= suggestedqueries.RequiredQueries; i++ {
		candidates = append(candidates, suggestedqueries.Candidate{Question: fmt.Sprintf("Question %d?", i), Intent: "learn", CorpusAnchorConceptIDs: []string{"c1"}, Generation: suggestedqueries.GenerationMetadata{Model: "m", PromptVersion: "p"}})
	}
	data, err := json.Marshal(suggestedqueries.ArtifactFromCandidates(candidates, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSuggestedCasesRejectIncompleteV2ArtifactBeforeGeneratingCases(t *testing.T) {
	data := validExperimentSuggestedQueries(t)
	data = bytes.Replace(data, []byte(`"Question 1?"`), []byte(`"different?"`), 1)
	if cases, err := suggestedCases(data, "wiki", nil); err == nil || cases != nil {
		t.Fatalf("incomplete or inconsistent artifact produced cases=%#v err=%v", cases, err)
	}
}

func TestSuggestedCasesRejectUnsupportedSchemaBeforeEffects(t *testing.T) {
	if _, err := suggestedCases([]byte(`{"version":1}`), "wiki", nil); err == nil {
		t.Fatal("unsupported schema accepted")
	}
}

type canceledSnapshotReader struct{}

func (canceledSnapshotReader) Prefix() string { return "test" }
func (canceledSnapshotReader) ReadFile(ctx context.Context, _ string) ([]byte, error) {
	return nil, ctx.Err()
}
func (canceledSnapshotReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	return nil, context.Canceled
}
func (canceledSnapshotReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, context.Canceled
}

func TestRemoteArtifactReadsPropagateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readRemoteArtifacts(ctx, canceledSnapshotReader{}, preparedSnapshot{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readRemoteArtifacts error = %v, want cancellation", err)
	}
}
