package main

import (
	"context"
	"errors"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/gcs"
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
	} {
		if _, err := parseGCSProjectRoot(raw); err == nil {
			t.Fatalf("parseGCSProjectRoot(%q) accepted malformed URI", raw)
		}
	}
}

func TestSuggestedCasesAreModeSpecificDeterministicAndBounded(t *testing.T) {
	data := []byte(`{"version":2,"queries":["one?","two?","three?"],"candidates":[` +
		`{"question":"One?","intent/use_case":"learn","corpus_anchor_concept_ids":["c1"],"generation":{"model":"m","prompt_version":"p"}},` +
		`{"question":"Two?","intent/use_case":"compare","corpus_anchor_concept_ids":["c2"],"generation":{"model":"m","prompt_version":"p"}},` +
		`{"question":"Three?","intent/use_case":"plan","corpus_anchor_concept_ids":["c3"],"generation":{"model":"m","prompt_version":"p"}}] ,"updated_at":"2026-08-20T00:00:00Z"}`)
	one, err := suggestedCases(data, "wiki", nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := suggestedCases(data, "full", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 3 || one[0].ID != "suggested-wiki-01" || one[0].Mode != "wiki" || one[0].Tags[2] != "intent:learn" {
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

func TestSuggestedCasesRejectUnsupportedSchemaBeforeEffects(t *testing.T) {
	if _, err := suggestedCases([]byte(`{"version":1}`), "wiki", nil); err == nil {
		t.Fatal("unsupported schema accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("test context was not canceled")
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
