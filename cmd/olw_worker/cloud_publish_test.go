package main

import (
	"bytes"
	cloudstorage "cloud.google.com/go/storage"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	_ "modernc.org/sqlite"
)

type memoryObject struct {
	data  []byte
	attrs objectAttrs
}
type memoryObjects struct {
	mu      sync.Mutex
	next    int64
	objects map[string]memoryObject
}

func newMemoryObjects() *memoryObjects { return &memoryObjects{objects: map[string]memoryObject{}} }
func (m *memoryObjects) Read(_ context.Context, name string, generation, limit int64) ([]byte, objectAttrs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[name]
	if !ok || generation > 0 && o.attrs.Generation != generation {
		return nil, objectAttrs{}, cloudstorage.ErrObjectNotExist
	}
	if o.attrs.Size > limit {
		return nil, objectAttrs{}, errors.New("object exceeds input limit")
	}
	return append([]byte(nil), o.data...), o.attrs, nil
}
func (m *memoryObjects) List(_ context.Context, prefix string, max int) ([]objectAttrs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []objectAttrs
	for name, o := range m.objects {
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix {
			if len(out) == max {
				return nil, errors.New("object list exceeds limit")
			}
			a := o.attrs
			a.Name = name
			out = append(out, a)
		}
	}
	return out, nil
}
func (m *memoryObjects) Write(_ context.Context, name string, data []byte, metadata map[string]string, c objectConditions) (objectAttrs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.objects[name]
	if c.DoesNotExist && exists {
		return objectAttrs{}, errObjectGenerationConflict
	}
	if c.GenerationMatch > 0 && (!exists || old.attrs.Generation != c.GenerationMatch) {
		return objectAttrs{}, errObjectGenerationConflict
	}
	m.next++
	a := objectAttrs{Name: name, Generation: m.next, Size: int64(len(data)), Metadata: metadata}
	m.objects[name] = memoryObject{append([]byte(nil), data...), a}
	return a, nil
}
func (m *memoryObjects) Delete(_ context.Context, name string, generation int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[name]
	if !ok || generation > 0 && o.attrs.Generation != generation {
		return errObjectGenerationConflict
	}
	delete(m.objects, name)
	return nil
}
func (*memoryObjects) Close() error { return nil }

type countingObjectStore struct {
	objectStore
	calls int
}

func (s *countingObjectStore) Read(ctx context.Context, name string, generation, limit int64) ([]byte, objectAttrs, error) {
	s.calls++
	return s.objectStore.Read(ctx, name, generation, limit)
}
func (s *countingObjectStore) List(ctx context.Context, prefix string, max int) ([]objectAttrs, error) {
	s.calls++
	return s.objectStore.List(ctx, prefix, max)
}
func (s *countingObjectStore) Write(ctx context.Context, name string, data []byte, meta map[string]string, c objectConditions) (objectAttrs, error) {
	s.calls++
	return s.objectStore.Write(ctx, name, data, meta, c)
}
func (s *countingObjectStore) Delete(ctx context.Context, name string, generation int64) error {
	s.calls++
	return s.objectStore.Delete(ctx, name, generation)
}

type noFullProjectListStore struct {
	objectStore
	prefix                           string
	listedGeneration, readHistorical bool
}

func (s *noFullProjectListStore) List(ctx context.Context, prefix string, max int) ([]objectAttrs, error) {
	if prefix == s.prefix || strings.HasPrefix(prefix, s.prefix+generation.Prefix) {
		s.listedGeneration = true
		return nil, errors.New("full project listing")
	}
	return s.objectStore.List(ctx, prefix, max)
}
func (s *noFullProjectListStore) Read(ctx context.Context, name string, generationID, limit int64) ([]byte, objectAttrs, error) {
	if strings.HasPrefix(name, s.prefix+generation.Prefix+"g_history/") {
		s.readHistorical = true
		return nil, objectAttrs{}, errors.New("historical generation read")
	}
	return s.objectStore.Read(ctx, name, generationID, limit)
}

type contextAwareDeleteStore struct{ objectStore }

func (s *contextAwareDeleteStore) Delete(ctx context.Context, name string, generation int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.objectStore.Delete(ctx, name, generation)
}

type timeoutDeleteStore struct {
	objectStore
	hasDeadline bool
}

type leaseDeleteProbe struct {
	objectStore
	failures       int
	attempts       int
	generations    []int64
	contextErrors  []error
	hasDeadline    []bool
	replaceOnFirst bool
}

func (s *leaseDeleteProbe) Delete(ctx context.Context, name string, objectGeneration int64) error {
	s.attempts++
	s.generations = append(s.generations, objectGeneration)
	s.contextErrors = append(s.contextErrors, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	s.hasDeadline = append(s.hasDeadline, hasDeadline)
	if s.replaceOnFirst && s.attempts == 1 {
		if _, err := s.objectStore.Write(ctx, name, []byte(`{"execution":"replacement"}`), nil, objectConditions{}); err != nil {
			return err
		}
	}
	if s.failures > 0 {
		s.failures--
		return errors.New("provider delete tenant project generation secret")
	}
	return s.objectStore.Delete(ctx, name, objectGeneration)
}

// commitThenErrorStore models a transport timeout after GCS committed the
// manifest. It can also make the required pointer readback ambiguous.
type commitThenErrorStore struct {
	objectStore
	manifest string
	unknown  bool
	wrote    bool
}

func (s *commitThenErrorStore) Write(ctx context.Context, name string, data []byte, meta map[string]string, condition objectConditions) (objectAttrs, error) {
	attrs, err := s.objectStore.Write(ctx, name, data, meta, condition)
	if err == nil && name == s.manifest {
		s.wrote = true
		return attrs, errors.New("transport timeout with provider detail")
	}
	return attrs, err
}

func (s *commitThenErrorStore) Read(ctx context.Context, name string, objectGeneration, limit int64) ([]byte, objectAttrs, error) {
	if s.unknown && s.wrote && name == s.manifest {
		return nil, objectAttrs{}, errors.New("provider pointer read unavailable")
	}
	return s.objectStore.Read(ctx, name, objectGeneration, limit)
}

func (s *timeoutDeleteStore) Delete(ctx context.Context, _ string, _ int64) error {
	_, s.hasDeadline = ctx.Deadline()
	return context.DeadlineExceeded
}

func TestCloudLeaseRejectsOverlapAndReleaseUsesGeneration(t *testing.T) {
	m := newMemoryObjects()
	first, err := acquireCloudLease(context.Background(), m, "p/", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireCloudLease(context.Background(), m, "p/", "y"); err == nil {
		t.Fatal("second lease succeeded")
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireCloudLease(context.Background(), m, "p/", "z"); err != nil {
		t.Fatal(err)
	}
}

func TestCloudPrimaryAndCleanupFailuresAreJoinedSanitized(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	prefix := "users/user-secret/projects/project-secret/"

	tests := []struct {
		name    string
		primary string
		setup   func(*memoryObjects) objectStore
		exec    func(string) error
		want    string
	}{
		{
			name:    "execution",
			primary: "pipeline execution failed",
			exec: func(string) error {
				return errors.New("provider execution secret /tmp/private")
			},
			want: "pipeline execution failed\npipeline cleanup failed",
		},
		{
			name:    "pre-commit publish",
			primary: "pipeline publish failed",
			setup: func(m *memoryObjects) objectStore {
				return &failureStore{objectStore: m, failWrite: func(name string, _ int) error {
					if strings.Contains(name, generation.Prefix) {
						return errors.New("provider publish tenant user-secret /tmp/private")
					}
					return nil
				}}
			},
			want: "pipeline publish failed\npipeline cleanup failed",
		},
		{
			name:    "post-commit receipt",
			primary: "pipeline committed but receipt recording failed",
			setup: func(m *memoryObjects) objectStore {
				return &failureStore{objectStore: m, failWrite: func(name string, _ int) error {
					if name == prefix+sourcestatus.Path {
						return errors.New("provider receipt project-secret /tmp/private")
					}
					return nil
				}}
			},
			want: "pipeline committed but receipt recording failed\npipeline cleanup failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemoryObjects()
			seedCloudSource(t, m, prefix, "raw-start", "", priorCloudReceipt())
			base := objectStore(m)
			if tc.setup != nil {
				base = tc.setup(m)
			}
			store := &leaseDeleteProbe{objectStore: base, failures: 3}
			execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
				if tc.exec != nil {
					if err := tc.exec(vault); err != nil {
						return err
					}
				}
				mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
				writeCloudRequiredOutputs(t, vault)
				return nil
			}
			err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, store)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error=%q, want %q", err, tc.want)
			}
			for _, forbidden := range []string{"provider", "secret", "user-secret", "project-secret", "execution-secret", "/tmp", "generation"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked %q: %q", forbidden, err)
				}
			}
			if store.attempts != 3 {
				t.Fatalf("cleanup attempts=%d, want 3", store.attempts)
			}
		})
	}
}

func TestCloudLeaseReleaseRetriesWithExactGenerationAndFreshContext(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		failures   int
	}{
		{name: "success on final attempt", failures: 2},
		{name: "exhausted", failures: 3, want: "pipeline cleanup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemoryObjects()
			store := &leaseDeleteProbe{objectStore: m, failures: tc.failures}
			lease, err := acquireCloudLease(context.Background(), store, "p/", "execution")
			if err != nil {
				t.Fatal(err)
			}
			acquiredGeneration := lease.generation
			caller, cancel := context.WithCancel(context.Background())
			cancel()
			err = lease.Release(caller)
			if tc.want == "" && err != nil {
				t.Fatalf("release failed: %v", err)
			}
			if tc.want != "" && (err == nil || err.Error() != tc.want) {
				t.Fatalf("release error=%q, want %q", err, tc.want)
			}
			if store.attempts != 3 {
				t.Fatalf("delete attempts=%d, want exact cap 3", store.attempts)
			}
			for i, got := range store.generations {
				if got != acquiredGeneration {
					t.Fatalf("attempt %d generation=%d, want %d", i+1, got, acquiredGeneration)
				}
				if store.contextErrors[i] != nil || !store.hasDeadline[i] {
					t.Fatalf("attempt %d did not receive fresh bounded context: err=%v deadline=%v", i+1, store.contextErrors[i], store.hasDeadline[i])
				}
			}
			if tc.want == "" {
				if _, _, err := m.Read(context.Background(), "p/"+generation.LeasePath, 0, generation.MaxFileBytes); !errors.Is(err, cloudstorage.ErrObjectNotExist) {
					t.Fatalf("lease remains after retry success: %v", err)
				}
			}
		})
	}
}

func TestCloudLeaseReleaseNeverDeletesReplacementGeneration(t *testing.T) {
	m := newMemoryObjects()
	store := &leaseDeleteProbe{objectStore: m, replaceOnFirst: true}
	lease, err := acquireCloudLease(context.Background(), store, "p/", "execution")
	if err != nil {
		t.Fatal(err)
	}
	oldGeneration := lease.generation
	if err := lease.Release(context.Background()); err == nil {
		t.Fatal("release unexpectedly deleted replacement lease")
	}
	if store.attempts != 3 {
		t.Fatalf("delete attempts=%d, want exact cap 3", store.attempts)
	}
	for _, got := range store.generations {
		if got != oldGeneration {
			t.Fatalf("replacement cleanup used generation=%d, want %d", got, oldGeneration)
		}
	}
	data, attrs, err := m.Read(context.Background(), "p/"+generation.LeasePath, 0, generation.MaxFileBytes)
	if err != nil || string(data) != `{"execution":"replacement"}` || attrs.Generation == oldGeneration {
		t.Fatalf("replacement lease=%q attrs=%+v err=%v", data, attrs, err)
	}
}

func TestDeploymentLeaseBreakGlassRunbookRequiresGenerationPrecondition(t *testing.T) {
	data, err := os.ReadFile("../../docs/DEPLOYMENT.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	start := strings.Index(doc, "## Stuck dev publish lease")
	if start < 0 {
		t.Fatal("missing stuck dev publish lease section")
	}
	section := doc[start:]
	for _, want := range []string{
		"RUNNING",
		".lwc/publish/lease.json",
		"exact object generation",
		"ifGenerationMatch",
		"concurrent publishers",
		"users/<user>/projects/<project>/",
		"https://storage.googleapis.com/storage/v1/",
		"DELETE",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("runbook missing %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(section), "verify absence") || !strings.Contains(section, "abort") || !strings.Contains(section, "generation changed") {
		t.Fatal("runbook must require abort-on-change and absence verification")
	}
	if strings.Contains(section, "llm-wiki-data") || strings.Contains(section, "llm-wiki-cloud") {
		t.Fatal("runbook section contains an environment-specific resource")
	}
}

func TestCloudManifestCommitTimeoutReadbackControlsReceipts(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	prefix := "users/user-secret/projects/project-secret/"

	t.Run("matching pointer is committed", func(t *testing.T) {
		m := newMemoryObjects()
		seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
		execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
			mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
			writeCloudRequiredOutputs(t, vault)
			return nil
		}
		store := &commitThenErrorStore{objectStore: m, manifest: prefix + generation.ManifestPath}
		if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, store); err != nil {
			t.Fatalf("committed timeout run: %v", err)
		}
		data, _, err := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := generation.Decode(data); err != nil {
			t.Fatalf("committed pointer invalid: %v", err)
		}
		if got := cloudStatus(t, m, prefix).Sources["s1"]; got.LastSuccessAt == "" || got.Error != "" {
			t.Fatalf("receipt after committed timeout = %+v", got)
		}
	})

	t.Run("ambiguous pointer leaves receipts untouched", func(t *testing.T) {
		m := newMemoryObjects()
		prior := priorCloudReceipt()
		seedCloudSource(t, m, prefix, "raw", "", prior)
		execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
			mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
			writeCloudRequiredOutputs(t, vault)
			return nil
		}
		store := &commitThenErrorStore{objectStore: m, manifest: prefix + generation.ManifestPath, unknown: true}
		err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, store)
		if err == nil || err.Error() != "manifest commit outcome unknown" {
			t.Fatalf("ambiguous timeout error = %v", err)
		}
		if got := cloudStatus(t, m, prefix).Sources["s1"]; got.LastIngestFingerprint != prior.LastIngestFingerprint || got.Error != "" {
			t.Fatalf("ambiguous timeout altered receipt = %+v", got)
		}
		if _, _, err := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes); err != nil {
			t.Fatalf("ambiguous timeout removed committed manifest: %v", err)
		}
	})
}

func TestCloudRejectsUnsafeCommandContractBeforeLeaseStorageOrChild(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      workerConfig
		commands [][]string
	}{
		{"no postprocess", workerConfig{Postprocess: false}, [][]string{{"run"}}},
		{"approve only", workerConfig{Postprocess: true}, [][]string{{"approve", "--all"}}},
		{"run is not first", workerConfig{Postprocess: true}, [][]string{{"clear"}, {"run"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &countingObjectStore{objectStore: newMemoryObjects()}
			cfg := cloudCfg()
			cfg.Postprocess = tc.cfg.Postprocess
			called := false
			old := execOLW
			defer func() { execOLW = old }()
			execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
				called = true
				return nil
			}
			err := runCloudWorkerBatch(context.Background(), cfg, tc.commands, store)
			if err == nil || err.Error() != "cloud worker configuration is invalid" {
				t.Fatalf("error=%v", err)
			}
			if store.calls != 0 || called {
				t.Fatalf("storage calls=%d child=%v, want neither", store.calls, called)
			}
		})
	}
}

func TestCloudRejectsInvalidExecutionIDBeforeAnyTouch(t *testing.T) {
	for _, executionID := range []string{"", " ", "../escape", "a/b"} {
		t.Run(fmt.Sprintf("id-%q", executionID), func(t *testing.T) {
			store := &countingObjectStore{objectStore: newMemoryObjects()}
			called := false
			old := execOLW
			defer func() { execOLW = old }()
			execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
				called = true
				return nil
			}
			cfg := cloudCfg()
			cfg.ExecutionID = executionID
			err := runCloudWorkerBatch(context.Background(), cfg, [][]string{{"run"}}, store)
			if err == nil || err.Error() != "cloud worker input is invalid" {
				t.Fatalf("error=%v, want invalid input", err)
			}
			if store.calls != 0 || called {
				t.Fatalf("storage calls=%d child=%v, want neither", store.calls, called)
			}
		})
	}
}

func TestCloudFailureRecordingPropagatesLogAndStatusFailures(t *testing.T) {
	prefix := "users/user-secret/projects/project-secret/"
	for _, tc := range []struct {
		name       string
		failLog    bool
		failStatus bool
	}{
		{name: "log", failLog: true},
		{name: "status", failStatus: true},
		{name: "both", failLog: true, failStatus: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemoryObjects()
			seedCloudSource(t, m, prefix, "raw-start", "", priorCloudReceipt())
			fail := &failureStore{objectStore: m, failWrite: func(name string, _ int) error {
				if tc.failLog && strings.Contains(name, "pipeline-") {
					return errors.New("provider log secret")
				}
				if tc.failStatus && name == prefix+sourcestatus.Path {
					return errors.New("provider status secret")
				}
				return nil
			}}
			old := execOLW
			defer func() { execOLW = old }()
			execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
				mustWriteFile(t, filepath.Join(vault, "cache", "pipeline-execution-secret.log"), []byte("child diagnostic"))
				return errors.New("primary provider failure")
			}
			err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, fail)
			if err == nil || !strings.Contains(err.Error(), "pipeline execution failed") || !strings.Contains(err.Error(), "failure state recording failed") {
				t.Fatalf("error=%v, want primary and recording categories", err)
			}
			if strings.Contains(err.Error(), "provider") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked recording cause: %v", err)
			}
		})
	}
}

func TestCloudPublishFailureWithRecordingFailurePreservesPublishCategory(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw-start", "", priorCloudReceipt())
	fail := &failureStore{objectStore: m, failWrite: func(name string, _ int) error {
		if strings.Contains(name, generation.Prefix) || strings.Contains(name, "pipeline-") || name == prefix+sourcestatus.Path {
			return errors.New("provider publish/status/log secret")
		}
		return nil
	}}
	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
		writeCloudRequiredOutputs(t, vault)
		return nil
	}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, fail)
	if err == nil || !strings.Contains(err.Error(), "pipeline publish failed") || !strings.Contains(err.Error(), "failure state recording failed") {
		t.Fatalf("error=%v, want publish and recording categories", err)
	}
	if strings.Contains(err.Error(), "provider") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked recording cause: %v", err)
	}
}

func TestCloudDiagnosticSinkDiscardsArbitraryChildOutput(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw-start", "", priorCloudReceipt())
	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(_ context.Context, vault string, _ []string, _ []string, stdout, stderr io.Writer) error {
		for _, value := range []string{
			"https://unknown-provider.invalid/resource",
			"/tmp/olw-cloud-sentinel/suffix",
			"tenant-secret project-secret execution-secret",
			"command --api-key=secret object/path generation/path",
			strings.Repeat("x", maxPipelineLog+1024),
		} {
			_, _ = io.WriteString(stdout, value)
			_, _ = io.WriteString(stderr, value)
		}
		return errors.New("child failed")
	}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m)
	if err == nil {
		t.Fatal("cloud run unexpectedly succeeded")
	}
	logData, _, readErr := m.Read(context.Background(), prefix+"cache/pipeline-execution-secret.log", 0, generation.MaxFileBytes)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(logData) != "pipeline failed\n" {
		t.Fatalf("cloud pipeline log was not fixed failure event: len=%d data=%q", len(logData), logData)
	}
	for _, forbidden := range []string{"unknown-provider.invalid", "olw-cloud-sentinel", "tenant-secret", "project-secret", "execution-secret", "command", "object/path", "generation/path"} {
		if strings.Contains(string(logData), forbidden) {
			t.Fatalf("cloud pipeline log retained child diagnostic %q: %q", forbidden, logData)
		}
	}
}

func TestWriteCloudPipelineLogUsesOnlyFixedEvent(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "pipeline-execution-secret.log"), []byte("https://unknown-provider.invalid/resource tenant-secret /tmp/workspace-suffix command --token=secret "+strings.Repeat("untrusted-bytes ", maxPipelineLog+1)))
	m := newMemoryObjects()
	cfg := cloudCfg()
	if err := writeCloudPipelineLog(context.Background(), m, "users/user/projects/project/", workspace, cfg); err != nil {
		t.Fatal(err)
	}
	data, _, err := m.Read(context.Background(), "users/user/projects/project/cache/pipeline-execution-secret.log", 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "pipeline completed\n" {
		t.Fatalf("pipeline log = %q, want fixed event", data)
	}
}

func TestCloudMaterializationReadsManifestDirectlyAndNeverListsGenerations(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
	seedCloudManifest(t, m, prefix, "old")
	for i := 0; i < generation.MaxFiles+10; i++ {
		writeCloudObject(t, m, prefix+generation.Prefix+"g_history/wiki/old-"+strconv.Itoa(i)+".md", []byte("old"))
	}
	store := &noFullProjectListStore{objectStore: m, prefix: prefix}
	workspace := t.TempDir()
	if _, _, _, err := materializeCloudWorkspace(context.Background(), store, prefix, workspace); err != nil {
		t.Fatal(err)
	}
	if store.listedGeneration || store.readHistorical {
		t.Fatalf("listed generation=%v read historical=%v", store.listedGeneration, store.readHistorical)
	}
}

func TestCloudMaterializationRejectsPresentEmptyManifestBeforeChild(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
	writeCloudObject(t, m, prefix+generation.ManifestPath, nil)
	called := false
	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m)
	if err == nil || err.Error() != "pipeline input materialization failed" || called {
		t.Fatalf("error=%v child=%v", err, called)
	}
}

func TestCloudMaterializationFailureRecordsFixedFailureReceipt(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw-start", "", priorCloudReceipt())
	writeCloudObject(t, m, prefix+generation.ManifestPath, []byte("{"))
	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		t.Fatal("child ran after materialization failure")
		return nil
	}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m)
	if err == nil || err.Error() != "pipeline input materialization failed" {
		t.Fatalf("error=%v, want fixed materialization failure", err)
	}
	logData, _, err := m.Read(context.Background(), prefix+"cache/pipeline-execution-secret.log", 0, generation.MaxFileBytes)
	if err != nil || string(logData) != "pipeline failed\n" {
		t.Fatalf("failure log=%q err=%v", logData, err)
	}
	receipt := cloudStatus(t, m, prefix).Sources["s1"]
	if receipt.Error != "pipeline failed" || receipt.FailedFingerprint == "" {
		t.Fatalf("materialization failure receipt=%+v", receipt)
	}
}

func TestCloudOversizeCanonicalInputFailsBeforeChild(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
	o := m.objects[prefix+"raw/source.md"]
	o.attrs.Size = generation.MaxFileBytes + 1
	m.objects[prefix+"raw/source.md"] = o
	called := false
	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m)
	if err == nil || err.Error() != "pipeline input materialization failed" || called {
		t.Fatalf("error=%v child=%v", err, called)
	}
}

func TestCloudReleaseUsesFreshContextAfterRunCancellation(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		cancel()
		return errors.New("canceled")
	}
	err := runCloudWorkerBatch(ctx, cloudCfg(), [][]string{{"run"}}, &contextAwareDeleteStore{objectStore: m})
	if err == nil || err.Error() != "pipeline execution failed" {
		t.Fatalf("error=%v", err)
	}
	if _, _, err := m.Read(context.Background(), prefix+generation.LeasePath, 0, generation.MaxFileBytes); !errors.Is(err, cloudstorage.ErrObjectNotExist) {
		t.Fatalf("lease remains after canceled run: %v", err)
	}
}

func TestCloudReleaseTimeoutIsGenericAfterCommit(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
	old := execOLW
	defer func() { execOLW = old }()
	execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
		writeCloudRequiredOutputs(t, vault)
		return nil
	}
	store := &timeoutDeleteStore{objectStore: m}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, store)
	if err == nil || err.Error() != "pipeline committed but cleanup failed" || !store.hasDeadline {
		t.Fatalf("error=%v fresh deadline=%v", err, store.hasDeadline)
	}
	if _, _, err := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes); err != nil {
		t.Fatalf("committed manifest lost: %v", err)
	}
}

func TestPublishCloudGenerationUsesImmutableFilesAndManifestCAS(t *testing.T) {
	root := t.TempDir()
	if err := writeCloudFile(root, "wiki/a.md", []byte("new")); err != nil {
		t.Fatal(err)
	}
	writeCloudRequiredOutputs(t, root)
	m := newMemoryObjects()
	got, _, err := publishCloudGeneration(context.Background(), m, "p/", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 7 {
		t.Fatalf("files=%d", len(got.Files))
	}
	if _, _, err := publishCloudGeneration(context.Background(), m, "p/", root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := generationOutputFiles(root); err != nil {
		t.Fatal(err)
	}
}

// These tests exercise the production cloud worker path.  The store is the
// only boundary substituted; OLW remains the existing test hook.
func TestCloudPreflightRejectsUnsafeOutputBeforeAnyGenerationWrite(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "wiki", "ok.md"), []byte("ok"))
	if err := os.Symlink(filepath.Join(root, "wiki", "ok.md"), filepath.Join(root, "wiki", "link.md")); err != nil {
		t.Fatal(err)
	}
	m := newMemoryObjects()
	if _, _, err := publishCloudGeneration(context.Background(), m, "p/", root, nil); err == nil {
		t.Fatal("publish accepted unsafe output")
	}
	if got, _ := m.List(context.Background(), "p/"+generation.Prefix, generation.MaxFiles); len(got) != 0 {
		t.Fatalf("generation writes=%+v, want none", got)
	}
}

func TestCloudPreflightStopsWalkAtMaxFilesPlusOne(t *testing.T) {
	root := t.TempDir()
	writeCloudRequiredOutputs(t, root)
	for i := 0; i < generation.MaxFiles; i++ {
		mustWriteFile(t, filepath.Join(root, "wiki", fmt.Sprintf("%05d.md", i)), []byte("x"))
	}
	oldWalk := walkGenerationDir
	defer func() { walkGenerationDir = oldWalk }()
	ownedVisits := 0
	walkGenerationDir = func(root string, visit fs.WalkDirFunc) error {
		return oldWalk(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				rel, _ := filepath.Rel(root, path)
				if generation.GenerationOwned(filepath.ToSlash(rel)) {
					ownedVisits++
				}
			}
			return visit(path, entry, err)
		})
	}
	if _, err := preflightGenerationOutputs(root); err == nil || err.Error() != "too many generation files" {
		t.Fatalf("preflight error = %v", err)
	}
	if ownedVisits != generation.MaxFiles+1 {
		t.Fatalf("owned visits = %d, want immediate stop at %d", ownedVisits, generation.MaxFiles+1)
	}
}

func TestCloudPreflightRejectsGlobalEntryBudgetOutsideGeneration(t *testing.T) {
	root := t.TempDir()
	writeCloudRequiredOutputs(t, root)
	for i := 0; i <= maxGenerationWorkspaceEntries; i++ {
		mustWriteFile(t, filepath.Join(root, "junk", fmt.Sprintf("%05d.txt", i)), []byte("x"))
	}
	if _, err := preflightGenerationOutputs(root); err == nil {
		t.Fatal("preflight accepted more than the global workspace entry budget")
	}
}

func TestCloudPreflightRejectsExcessiveTraversalDepth(t *testing.T) {
	root := t.TempDir()
	writeCloudRequiredOutputs(t, root)
	deep := filepath.Join(root, strings.Repeat("nested/", 65))
	mustWriteFile(t, filepath.Join(deep, "junk.txt"), []byte("x"))
	if _, err := preflightGenerationOutputs(root); err == nil {
		t.Fatal("preflight accepted excessive workspace traversal depth")
	}
}

func TestCloudPreflightRejectsDirectoryAndTraversalByteBudgets(t *testing.T) {
	root := t.TempDir()
	writeCloudRequiredOutputs(t, root)
	if err := os.MkdirAll(filepath.Join(root, "junk", "nested", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	limits := generationTraversalLimits{entries: 100, directories: 2, depth: 64, bytes: generation.MaxTotalSize}
	if _, err := preflightGenerationOutputsWithLimits(root, limits); err == nil {
		t.Fatal("preflight accepted excessive directory traversal")
	}

	root = t.TempDir()
	writeCloudRequiredOutputs(t, root)
	junk := filepath.Join(root, "junk", "large.bin")
	if err := os.MkdirAll(filepath.Dir(junk), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(junk, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(generation.MaxTotalSize + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := preflightGenerationOutputs(root); err == nil {
		t.Fatal("preflight accepted excessive cumulative traversal bytes")
	}
}

func TestCloudPreCommitFailuresKeepOldManifestAndRecordSanitizedFailure(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw-start", "annotation-start", priorCloudReceipt())
	oldManifest := seedCloudManifest(t, m, prefix, "old")
	fail := &failureStore{objectStore: m, failWrite: func(name string, _ int) error {
		if strings.Contains(name, generation.Prefix) {
			return errors.New("provider failure /tmp/private tenant user-secret")
		}
		return nil
	}}
	execOLW = func(_ context.Context, vault string, command []string, _ []string, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stdout, "api-secret user-secret ")
		_, _ = io.WriteString(stderr, "project-secret --dangerous-arg\n")
		mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
		writeCloudRequiredOutputs(t, vault)
		return nil
	}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run", "--dangerous-arg"}}, fail)
	if err == nil || strings.Contains(err.Error(), "user-secret") || strings.Contains(err.Error(), "provider") {
		t.Fatalf("error=%q is not generic", err)
	}
	got, _, readErr := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
	if readErr != nil || !bytes.Equal(got, oldManifest) {
		t.Fatalf("manifest=%q err=%v, want byte-identical old", got, readErr)
	}
	if !cloudSnapshotCurrent(context.Background(), m, prefix, sourceSnapshot{SourceID: "s1", RawPath: "raw/source.md", RawSHA256: sha256Text("raw-start"), AnnotationSHA: annotation.Digest("annotation-start")}) {
		t.Fatal("seed source is unexpectedly not current")
	}
	assertCloudFailure(t, m, prefix, "api-secret", "user-secret", "project-secret", "--dangerous-arg")
}

func TestCloudPostCommitReceiptFailureDoesNotRollbackManifest(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw-start", "", priorCloudReceipt())
	oldManifest := seedCloudManifest(t, m, prefix, "old")
	fail := &failureStore{objectStore: m, failWrite: func(name string, n int) error {
		if name == prefix+sourcestatus.Path && n > 0 {
			return errors.New("receipt provider /tmp/private")
		}
		return nil
	}}
	execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
		writeCloudRequiredOutputs(t, vault)
		return nil
	}
	err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run", "--secret-arg"}}, fail)
	if err == nil || !strings.Contains(err.Error(), "receipt") || strings.Contains(err.Error(), "private") {
		t.Fatalf("error=%q, want generic post-commit receipt error", err)
	}
	got, _, readErr := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
	if readErr != nil || bytes.Equal(got, oldManifest) {
		t.Fatalf("manifest not committed: %q err=%v", got, readErr)
	}
}

func TestCloudSuccessUsesExactStartAndConcurrentChangesStayDirty(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	seedCloudSource(t, m, prefix, "raw-start", "annotation-start", sourcestatus.Receipt{})
	execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
		writeCloudRequiredOutputs(t, vault)
		writeCloudObject(t, m, prefix+"raw/source.md", []byte("raw-concurrent"))
		writeCloudObject(t, m, prefix+annotation.Path("s1"), cloudAnnotation(t, "annotation-concurrent"))
		return nil
	}
	if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m); err != nil {
		t.Fatal(err)
	}
	status := cloudStatus(t, m, prefix)
	if got := status.Sources["s1"]; got.LastSuccessAt != "" || got.FailedFingerprint != "" {
		t.Fatalf("concurrent source received success/failure for stale snapshot: %+v", got)
	}
	raw, _, _ := m.Read(context.Background(), prefix+"raw/source.md", 0, generation.MaxFileBytes)
	if string(raw) != "raw-concurrent" {
		t.Fatalf("raw changed: %q", raw)
	}
}

func TestCloudManifestConflictKeepsOldManifestWithoutSuccessReceipt(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	prior := priorCloudReceipt()
	seedCloudSource(t, m, prefix, "raw-start", "", prior)
	oldManifest := seedCloudManifest(t, m, prefix, "old")
	fail := &failureStore{objectStore: m, failWrite: func(name string, _ int) error {
		if name == prefix+generation.ManifestPath {
			return errors.New("manifest cas conflict /private/secret")
		}
		return nil
	}}
	execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
		writeCloudRequiredOutputs(t, vault)
		return nil
	}
	if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, fail); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error=%v", err)
	}
	got, _, _ := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes)
	if !bytes.Equal(got, oldManifest) {
		t.Fatal("manifest changed after CAS conflict")
	}
	if receipt := cloudStatus(t, m, prefix).Sources["s1"]; receipt.LastIngestFingerprint != prior.LastIngestFingerprint {
		t.Fatalf("success receipt changed: %+v", receipt)
	}
}

func TestCloudLeaseOverlapDoesNotMaterializeOrExecute(t *testing.T) {
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	lease, err := acquireCloudLease(context.Background(), m, prefix, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(context.Background())
	old := execOLW
	defer func() { execOLW = old }()
	called := false
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("error=%v", err)
	}
	if called {
		t.Fatal("overlap called OLW")
	}
	if got, _ := m.List(context.Background(), prefix+generation.Prefix, generation.MaxFiles); len(got) != 0 {
		t.Fatalf("overlap wrote generation: %+v", got)
	}
}

func TestCloudRejectsOversizeInputBeforeLeaseOrChild(t *testing.T) {
	m := newMemoryObjects()
	cfg := cloudCfg()
	cfg.APIKey = strings.Repeat("x", maxWorkerKeyBytes+1)
	old := execOLW
	defer func() { execOLW = old }()
	called := false
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		called = true
		return nil
	}
	err := runCloudWorkerBatch(context.Background(), cfg, [][]string{{"run"}}, m)
	if err == nil || strings.Contains(err.Error(), "x") {
		t.Fatalf("error=%v", err)
	}
	if called {
		t.Fatal("oversize input called child")
	}
	if got, _ := m.List(context.Background(), "", generation.MaxFiles); len(got) != 0 {
		t.Fatalf("oversize input acquired lease: %+v", got)
	}
}

func TestCloudReceiptCASRetriesOnlyConflictsAndPreservesConcurrentReceipts(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	m := newMemoryObjects()
	prefix := "users/user-secret/projects/project-secret/"
	start := sourceSnapshot{SourceID: "s1", RawPath: "raw/source.md", RawSHA256: sha256Text("raw"), AnnotationSHA: annotation.Digest(""), Fingerprint: sourcestatus.Fingerprint(sha256Text("raw"), annotation.Digest(""))}
	seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
	execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
		writeCloudRequiredOutputs(t, vault)
		return nil
	}

	conflict := &receiptContentionStore{objectStore: m, name: prefix + sourcestatus.Path, mutate: func() {
		current := cloudStatus(t, m, prefix)
		current.Sources["other"] = sourcestatus.Receipt{RawPath: "raw/other.md", LastIngestFingerprint: strings.Repeat("d", 64), LastSuccessAt: "2026-01-01T00:00:00Z"}
		data, _ := json.Marshal(current)
		writeCloudObject(t, m, prefix+sourcestatus.Path, data)
	}}
	if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.conflicts != 1 || conflict.writes != 2 {
		t.Fatalf("conflicts=%d writes=%d, want one conflict then success", conflict.conflicts, conflict.writes)
	}
	status := cloudStatus(t, m, prefix)
	if status.Sources["other"].LastIngestFingerprint != strings.Repeat("d", 64) {
		t.Fatalf("unrelated receipt lost: %+v", status.Sources)
	}
	if status.Sources["s1"].LastIngestFingerprint != start.Fingerprint {
		t.Fatalf("source receipt=%+v", status.Sources["s1"])
	}

	nonConflict := &failureStore{objectStore: m, failWrite: func(name string, _ int) error {
		if name == prefix+sourcestatus.Path {
			return errors.New("provider failure")
		}
		return nil
	}}
	err := mergeCloudSuccess(context.Background(), nonConflict, prefix, []sourceSnapshot{start})
	if err == nil || err.Error() != "source receipt write failed" || nonConflict.writes[prefix+sourcestatus.Path] != 1 {
		t.Fatalf("err=%v writes=%d, want one generic non-conflict failure", err, nonConflict.writes[prefix+sourcestatus.Path])
	}
}

func TestMergeCloudReceiptsNormalizesExistingSentinels(t *testing.T) {
	m := newMemoryObjects()
	prefix := "p/"
	artifact := sourcestatus.Artifact{Version: 1, Sources: map[string]sourcestatus.Receipt{
		"safe": {
			RawPath:               "raw/safe.md",
			LastIngestedRawSHA256: strings.Repeat("a", 64),
			LastIngestedAnnSHA256: strings.Repeat("b", 64),
			LastIngestFingerprint: strings.Repeat("c", 64),
			LastSuccessAt:         "2026-01-01T00:00:00Z",
		},
		"sentinel-source": {
			RawPath:           "raw/sentinel.md",
			FailedFingerprint: "not-a-fingerprint",
			Error:             "provider https://unknown-provider.invalid/tenant-secret /tmp/workspace-secret",
		},
	}}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	writeCloudObject(t, m, prefix+sourcestatus.Path, data)
	if err := mergeCloudReceipts(context.Background(), m, prefix, func(*sourcestatus.Artifact) {}); err != nil {
		t.Fatal(err)
	}
	got := cloudStatus(t, m, prefix)
	if got.Sources["safe"].LastSuccessAt != artifact.Sources["safe"].LastSuccessAt {
		t.Fatalf("valid receipt changed: %+v", got.Sources["safe"])
	}
	bad := got.Sources["sentinel-source"]
	if bad.Error != "pipeline failed" || bad.FailedFingerprint != "" || strings.Contains(string(mustReceiptJSON(t, got)), "unknown-provider") {
		t.Fatalf("existing receipt was not normalized: %+v", bad)
	}
}

func mustReceiptJSON(t *testing.T, artifact sourcestatus.Artifact) []byte {
	t.Helper()
	b, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCloudReceiptCASExhaustionIsGenericAndFailurePreservesSuccess(t *testing.T) {
	m := newMemoryObjects()
	prefix := "p/"
	start := sourceSnapshot{SourceID: "s1", RawPath: "raw/source.md", RawSHA256: sha256Text("raw"), AnnotationSHA: annotation.Digest(""), Fingerprint: sourcestatus.Fingerprint(sha256Text("raw"), annotation.Digest(""))}
	prior := priorCloudReceipt()
	seedCloudSource(t, m, prefix, "raw", "", prior)
	exhausted := &receiptContentionStore{objectStore: m, name: prefix + sourcestatus.Path, always: true}
	if err := mergeCloudSuccess(context.Background(), exhausted, prefix, []sourceSnapshot{start}); err == nil || err.Error() != "source receipt conflict" {
		t.Fatalf("success exhaustion error=%v", err)
	}
	if exhausted.writes != cloudReceiptCASAttempts {
		t.Fatalf("success conflict attempts=%d, want %d", exhausted.writes, cloudReceiptCASAttempts)
	}
	if got := cloudStatus(t, m, prefix).Sources["s1"]; got.LastIngestFingerprint != prior.LastIngestFingerprint {
		t.Fatalf("exhaustion changed receipt: %+v", got)
	}
	exhausted = &receiptContentionStore{objectStore: m, name: prefix + sourcestatus.Path, always: true}
	if err := mergeCloudFailure(context.Background(), exhausted, prefix, []sourceSnapshot{start}); err == nil || err.Error() != "source receipt conflict" {
		t.Fatalf("failure exhaustion error=%v", err)
	}
	if exhausted.writes != cloudReceiptCASAttempts {
		t.Fatalf("failure conflict attempts=%d, want %d", exhausted.writes, cloudReceiptCASAttempts)
	}
	if got := cloudStatus(t, m, prefix).Sources["s1"]; got.LastSuccessAt != prior.LastSuccessAt || got.LastIngestFingerprint != prior.LastIngestFingerprint {
		t.Fatalf("failure exhaustion lost success fields: %+v", got)
	}
}

func TestCloudLeaseReleaseConflictsPreserveExactState(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	prefix := "users/user-secret/projects/project-secret/"

	t.Run("before commit", func(t *testing.T) {
		m := newMemoryObjects()
		seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
		store := &deleteFailureStore{objectStore: m, fail: true, replace: func(ctx context.Context, name string) {
			if _, err := m.Write(ctx, name, []byte(`{"execution":"other"}`), nil, objectConditions{}); err != nil {
				t.Fatal(err)
			}
		}}
		execOLW = func(_ context.Context, _ string, _ []string, _ []string, _, _ io.Writer) error {
			return errors.New("child failure")
		}
		err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, store)
		if err == nil || err.Error() != "pipeline execution failed\npipeline cleanup failed" {
			t.Fatalf("error=%v", err)
		}
		if _, _, err := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes); !errors.Is(err, cloudstorage.ErrObjectNotExist) {
			t.Fatalf("manifest exists before commit: %v", err)
		}
		if got := cloudStatus(t, m, prefix).Sources["s1"]; got.LastIngestFingerprint != priorCloudReceipt().LastIngestFingerprint {
			t.Fatalf("false success receipt: %+v", got)
		}
		lease, _, err := m.Read(context.Background(), prefix+generation.LeasePath, 0, generation.MaxFileBytes)
		if err != nil || string(lease) != `{"execution":"other"}` {
			t.Fatalf("failed conditional release changed replacement lease=%q err=%v", lease, err)
		}
	})

	t.Run("after commit", func(t *testing.T) {
		m := newMemoryObjects()
		seedCloudSource(t, m, prefix, "raw", "", priorCloudReceipt())
		store := &deleteFailureStore{objectStore: m, fail: true, replace: func(ctx context.Context, name string) {
			if _, err := m.Write(ctx, name, []byte(`{"execution":"other"}`), nil, objectConditions{}); err != nil {
				t.Fatal(err)
			}
		}}
		execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
			mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
			writeCloudRequiredOutputs(t, vault)
			return nil
		}
		err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, store)
		if err == nil || err.Error() != "pipeline committed but cleanup failed" {
			t.Fatalf("error=%v", err)
		}
		if _, _, err := m.Read(context.Background(), prefix+generation.ManifestPath, 0, generation.MaxManifestBytes); err != nil {
			t.Fatalf("committed manifest lost: %v", err)
		}
		if got := cloudStatus(t, m, prefix).Sources["s1"]; got.LastIngestFingerprint == priorCloudReceipt().LastIngestFingerprint || got.Error != "" {
			t.Fatalf("success receipt replaced after cleanup conflict: %+v", got)
		}
		lease, _, err := m.Read(context.Background(), prefix+generation.LeasePath, 0, generation.MaxFileBytes)
		if err != nil || string(lease) != `{"execution":"other"}` {
			t.Fatalf("failed conditional release changed replacement lease=%q err=%v", lease, err)
		}
	})
}

func TestCloudSuccessExactStartRawAndAnnotationPairs(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	tests := []struct {
		name, startRaw, startAnnotation, currentRaw, currentAnnotation string
		success                                                        bool
	}{
		{"unchanged nonempty", "raw", "A", "raw", "A", true},
		{"annotation A to B", "raw", "A", "raw", "B", false},
		{"annotation nonempty to clear", "raw", "A", "raw", "", false},
		{"annotation clear to nonempty", "raw", "", "raw", "A", false},
		{"unchanged clear", "raw", "", "raw", "", true},
		{"raw changed", "raw A", "A", "raw B", "A", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemoryObjects()
			prefix := "users/user-secret/projects/project-secret/"
			prior := priorCloudReceipt()
			seedCloudSource(t, m, prefix, tc.startRaw, tc.startAnnotation, prior)
			var expectedRaw, expectedAnnotation []byte
			var expectedRawAttrs, expectedAnnotationAttrs objectAttrs
			captureCanonical := func() {
				var err error
				expectedRaw, expectedRawAttrs, err = m.Read(context.Background(), prefix+"raw/source.md", 0, generation.MaxFileBytes)
				if err != nil {
					t.Fatal(err)
				}
				expectedAnnotation, expectedAnnotationAttrs, err = m.Read(context.Background(), prefix+annotation.Path("s1"), 0, generation.MaxFileBytes)
				if errors.Is(err, cloudstorage.ErrObjectNotExist) {
					expectedAnnotation, expectedAnnotationAttrs = nil, objectAttrs{}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			execOLW = func(_ context.Context, vault string, _ []string, _ []string, _, _ io.Writer) error {
				mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("new"))
				writeCloudRequiredOutputs(t, vault)
				if tc.currentRaw != tc.startRaw {
					writeCloudObject(t, m, prefix+"raw/source.md", []byte(tc.currentRaw))
				}
				if tc.currentAnnotation != tc.startAnnotation {
					if tc.currentAnnotation == "" {
						if err := m.Delete(context.Background(), prefix+annotation.Path("s1"), 0); err != nil && !errors.Is(err, cloudstorage.ErrObjectNotExist) {
							t.Fatal(err)
						}
					} else {
						writeCloudObject(t, m, prefix+annotation.Path("s1"), cloudAnnotation(t, tc.currentAnnotation))
					}
				}
				captureCanonical()
				return nil
			}
			if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run"}}, m); err != nil {
				t.Fatal(err)
			}
			got := cloudStatus(t, m, prefix).Sources["s1"]
			if tc.success && got.LastIngestFingerprint != sourcestatus.Fingerprint(sha256Text(tc.startRaw), annotation.Digest(tc.startAnnotation)) {
				t.Fatalf("unchanged pair did not receive success: %+v", got)
			}
			if !tc.success && got.LastIngestFingerprint != prior.LastIngestFingerprint {
				t.Fatalf("changed pair was falsely marked successful: %+v", got)
			}
			raw, rawAttrs, err := m.Read(context.Background(), prefix+"raw/source.md", 0, generation.MaxFileBytes)
			if err != nil || !bytes.Equal(raw, expectedRaw) || rawAttrs.Generation != expectedRawAttrs.Generation {
				t.Fatalf("raw=%q attrs=%+v, want %q attrs=%+v err=%v", raw, rawAttrs, expectedRaw, expectedRawAttrs, err)
			}
			if tc.currentAnnotation == "" {
				if _, _, err := m.Read(context.Background(), prefix+annotation.Path("s1"), 0, generation.MaxFileBytes); !errors.Is(err, cloudstorage.ErrObjectNotExist) {
					t.Fatalf("annotation=%v, want absent", err)
				}
			} else if ann, attrs, err := m.Read(context.Background(), prefix+annotation.Path("s1"), 0, generation.MaxFileBytes); err != nil || !bytes.Equal(ann, expectedAnnotation) || attrs.Generation != expectedAnnotationAttrs.Generation {
				t.Fatalf("annotation=%q attrs=%+v, want %q attrs=%+v err=%v", ann, attrs, expectedAnnotation, expectedAnnotationAttrs, err)
			}
		})
	}
}

type failureStore struct {
	objectStore
	mu        sync.Mutex
	writes    map[string]int
	failWrite func(string, int) error
}

// receiptContentionStore advances source_status after the worker has read it,
// then delegates the stale GenerationMatch write to the production in-memory
// object store.  This models the real GCS precondition rather than faking an
// arbitrary write error.
type receiptContentionStore struct {
	objectStore
	name              string
	mutate            func()
	always            bool
	conflicts, writes int
}

func (s *receiptContentionStore) Write(ctx context.Context, name string, data []byte, meta map[string]string, condition objectConditions) (objectAttrs, error) {
	if name != s.name || (condition.GenerationMatch == 0 && !condition.DoesNotExist) {
		return s.objectStore.Write(ctx, name, data, meta, condition)
	}
	s.writes++
	if !s.always && s.conflicts > 0 {
		return s.objectStore.Write(ctx, name, data, meta, condition)
	}
	s.conflicts++
	if s.mutate != nil {
		s.mutate()
	} else {
		current, _, err := s.objectStore.Read(ctx, name, 0, generation.MaxFileBytes)
		if err != nil {
			return objectAttrs{}, err
		}
		if _, err := s.objectStore.Write(ctx, name, current, nil, objectConditions{}); err != nil {
			return objectAttrs{}, err
		}
	}
	return s.objectStore.Write(ctx, name, data, meta, condition)
}

type deleteFailureStore struct {
	objectStore
	fail    bool
	replace func(context.Context, string)
}

func (s *deleteFailureStore) Delete(ctx context.Context, name string, generation int64) error {
	if s.fail && generation > 0 {
		if s.replace != nil {
			s.replace(ctx, name)
		}
		return s.objectStore.Delete(ctx, name, generation)
	}
	return s.objectStore.Delete(ctx, name, generation)
}

func (s *failureStore) Write(ctx context.Context, name string, data []byte, meta map[string]string, condition objectConditions) (objectAttrs, error) {
	s.mu.Lock()
	if s.writes == nil {
		s.writes = map[string]int{}
	}
	s.writes[name]++
	n := s.writes[name]
	s.mu.Unlock()
	if err := s.failWrite(name, n); err != nil {
		return objectAttrs{}, err
	}
	return s.objectStore.Write(ctx, name, data, meta, condition)
}

func cloudCfg() workerConfig {
	return workerConfig{Bucket: "bucket", UserID: "user-secret", ProjectID: "project-secret", ExecutionID: "execution-secret", APIKey: "api-secret", WorkspaceDir: os.TempDir(), Postprocess: true, StopOnError: true, SuppressOutput: true}
}
func writeCloudRequiredOutputs(t *testing.T, root string) {
	t.Helper()
	for path, data := range map[string]string{
		"wiki.toml":                    "name = \"test\"\n",
		"cache/id_map.json":            `{"source_meta":{"s1":{"source_file":"raw/source.md"}}}`,
		"cache/concepts.jsonl":         "",
		"cache/raw_status.json":        "{}",
		"cache/suggested_queries.json": "{}",
	} {
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(path)), []byte(data))
	}
	statePath := filepath.Join(root, ".olw", "state.db")
	if err := os.MkdirAll(filepath.Dir(statePath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table raw_notes (path text primary key, content_hash text not null, status text not null default 'new', ingested_at text, error text);`); err != nil {
		t.Fatal(err)
	}
}
func writeCloudObject(t *testing.T, m *memoryObjects, name string, data []byte) {
	t.Helper()
	if _, err := m.Write(context.Background(), name, data, nil, objectConditions{}); err != nil {
		t.Fatal(err)
	}
}
func cloudAnnotation(t *testing.T, body string) []byte {
	t.Helper()
	b, err := json.Marshal(annotation.Object{Version: 1, SourceID: "s1", RawPath: "raw/source.md", Body: body, SHA256: annotation.Digest(body), UpdatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedBy: "safe"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func seedCloudSource(t *testing.T, m *memoryObjects, prefix, raw, ann string, receipt sourcestatus.Receipt) {
	t.Helper()
	writeCloudObject(t, m, prefix+"raw/source.md", []byte(raw))
	writeCloudObject(t, m, prefix+"cache/id_map.json", []byte(`{"source_meta":{"s1":{"source_file":"raw/source.md"}}}`))
	if ann != "" {
		writeCloudObject(t, m, prefix+annotation.Path("s1"), cloudAnnotation(t, ann))
	}
	if receipt.RawPath != "" {
		b, _ := json.Marshal(sourcestatus.Artifact{Version: 1, Sources: map[string]sourcestatus.Receipt{"s1": receipt}})
		writeCloudObject(t, m, prefix+sourcestatus.Path, b)
	}
}
func priorCloudReceipt() sourcestatus.Receipt {
	return sourcestatus.Receipt{RawPath: "raw/source.md", LastIngestedRawSHA256: "old", LastIngestedAnnSHA256: "old", LastIngestFingerprint: sourcestatus.Fingerprint("old", "old"), LastSuccessAt: time.Now().UTC().Format(time.RFC3339)}
}
func seedCloudManifest(t *testing.T, m *memoryObjects, prefix, content string) []byte {
	t.Helper()
	a, err := m.Write(context.Background(), prefix+generation.Prefix+"g_oldmanifest/wiki/old.md", []byte(content), map[string]string{"sha256": digestBytes([]byte(content))}, objectConditions{})
	if err != nil {
		t.Fatal(err)
	}
	f, err := generation.NewFile("wiki/old.md", []byte(content), a.Generation)
	if err != nil {
		t.Fatal(err)
	}
	idMap := []byte(`{"source_meta":{"s1":{"source_file":"raw/source.md"}}}`)
	mapAttrs, err := m.Write(context.Background(), prefix+generation.Prefix+"g_oldmanifest/cache/id_map.json", idMap, map[string]string{"sha256": digestBytes(idMap)}, objectConditions{})
	if err != nil {
		t.Fatal(err)
	}
	fMap, err := generation.NewFile("cache/id_map.json", idMap, mapAttrs.Generation)
	if err != nil {
		t.Fatal(err)
	}
	manifest := generation.Manifest{Version: generation.Version, GenerationID: "g_oldmanifest", CreatedAt: time.Now().UTC().Format(time.RFC3339), InputFingerprint: "start", Files: []generation.File{fMap, f}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeCloudObject(t, m, prefix+generation.ManifestPath, data)
	return data
}
func cloudStatus(t *testing.T, m *memoryObjects, prefix string) sourcestatus.Artifact {
	t.Helper()
	b, _, err := m.Read(context.Background(), prefix+sourcestatus.Path, 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	a, err := sourcestatus.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func assertCloudFailure(t *testing.T, m *memoryObjects, prefix string, forbidden ...string) {
	t.Helper()
	log, _, err := m.Read(context.Background(), prefix+"cache/pipeline-execution-secret.log", 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := m.Read(context.Background(), prefix+sourcestatus.Path, 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	all := string(log) + string(status)
	for _, value := range forbidden {
		if strings.Contains(all, value) {
			t.Fatalf("persisted raw %q in %q", value, all)
		}
	}
	receipt := cloudStatus(t, m, prefix).Sources["s1"]
	if receipt.Error == "" || receipt.LastSuccessAt == "" {
		t.Fatalf("failure receipt lost prior success: %+v", receipt)
	}
}
