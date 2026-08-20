package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/config"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/search"
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

func TestRemoteArtifactReadsValidatePublishedSuggestedQueriesWithoutMode(t *testing.T) {
	for _, count := range []int{3, suggestedqueries.RequiredQueries} {
		data := validExperimentSuggestedQueries(t)
		if count == 3 {
			artifact, err := suggestedqueries.Decode(data)
			if err != nil {
				t.Fatal(err)
			}
			artifact.Candidates = artifact.Candidates[:3]
			artifact.Queries = artifact.Queries[:3]
			data, err = json.Marshal(artifact)
			if err != nil {
				t.Fatal(err)
			}
		}
		prepared, err := readRemoteArtifacts(context.Background(), &remoteArtifactReader{
			files: map[string][]byte{conceptsPath: []byte(`{"slug":"coffee","title":"Coffee"}` + "\n"), suggestedPath: data},
		}, preparedSnapshot{})
		if err != nil || len(prepared.suggestedData) == 0 {
			t.Fatalf("count=%d prepared=%#v err=%v", count, prepared, err)
		}
	}

	_, err := readRemoteArtifacts(context.Background(), &remoteArtifactReader{files: map[string][]byte{
		conceptsPath: []byte(`{"slug":"coffee","title":"Coffee"}` + "\n"), suggestedPath: []byte(`{"version":1}`),
	}}, preparedSnapshot{})
	if err == nil {
		t.Fatal("malformed suggested artifact accepted without mode")
	}
}

type remoteArtifactReader struct {
	files  map[string][]byte
	active *bool
	reads  int
}

func (r *remoteArtifactReader) Prefix() string { return "remote-test" }
func (r *remoteArtifactReader) ReadFile(_ context.Context, path string) ([]byte, error) {
	if r.active != nil && !*r.active {
		return nil, errors.New("reader closed")
	}
	r.reads++
	data, ok := r.files[path]
	if !ok {
		return nil, errors.New("missing")
	}
	return data, nil
}
func (r *remoteArtifactReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	return nil, errors.New("not used")
}
func (r *remoteArtifactReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, errors.New("not used")
}

func TestRunExperimentClosesRemoteSnapshotAfterExecutionAndOnFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		loadConfig func(string) (config.Config, error)
		wantRead   bool
	}{
		{name: "success", loadConfig: func(string) (config.Config, error) { return config.Config{}, nil }, wantRead: true},
		{name: "config failure", loadConfig: func(string) (config.Config, error) { return config.Config{}, errors.New("config failed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			active := true
			closeCalls := 0
			reader := &remoteArtifactReader{active: &active, files: map[string][]byte{conceptsPath: []byte("concepts")}}
			casesPath := filepath.Join(t.TempDir(), "cases.jsonl")
			writeTestFile(t, casesPath, `{"id":"one","query":"coffee","mode":"wiki"}`+"\n")
			var output bytes.Buffer
			err := runExperiment(context.Background(), experimentOptions{snapshotPath: "gs://bucket/users/user/projects/project", casesPath: casesPath, runs: 1}, dependencies{
				loadConfig: test.loadConfig,
				newExecutor: func(*cache.Cache, config.Config) (query.Executor, error) {
					return readerCheckingExecutor{reader: reader}, nil
				},
				loadGCSSnapshot: func(context.Context, string, func(string) (*gcs.Client, error)) (preparedSnapshot, error) {
					return preparedSnapshot{reader: reader, cache: cache.New(), cleanup: func() error {
						closeCalls++
						active = false
						return nil
					}}, nil
				},
				now: time.Now, stdout: &output,
			})
			if test.wantRead && err != nil {
				t.Fatal(err)
			}
			if !test.wantRead && err == nil {
				t.Fatal("expected config failure")
			}
			if closeCalls != 1 || active {
				t.Fatalf("closeCalls=%d active=%v err=%v", closeCalls, active, err)
			}
			if test.wantRead && !readerReadOK(reader) {
				t.Fatal("executor did not read while snapshot was alive")
			}
		})
	}
}

type readerCheckingExecutor struct{ reader *remoteArtifactReader }

func (e readerCheckingExecutor) Execute(ctx context.Context, reader cache.Reader, request query.Request) (query.Result, error) {
	if reader != e.reader {
		return query.Result{}, errors.New("unexpected snapshot reader")
	}
	_, err := e.reader.ReadFile(ctx, conceptsPath)
	if err != nil {
		return query.Result{}, err
	}
	return query.Result{Query: request.Query, Mode: request.Mode, Results: []search.Result{{Slug: "coffee", Title: "Coffee", Type: "concept"}}}, nil
}

func readerReadOK(reader *remoteArtifactReader) bool {
	return reader.reads > 0
}
