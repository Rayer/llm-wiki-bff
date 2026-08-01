package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/generation"
)

type fixedLivenessProbe struct {
	state leaseHolderLiveness
	calls atomic.Int32
}

func (p *fixedLivenessProbe) Probe(_ context.Context, _, _ string) leaseHolderLiveness {
	p.calls.Add(1)
	return p.state
}

type mapLivenessProbe struct {
	mu    sync.Mutex
	byID  map[string]leaseHolderLiveness
	calls atomic.Int32
}

func (p *mapLivenessProbe) Probe(_ context.Context, executionID, _ string) leaseHolderLiveness {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byID == nil {
		return leaseHolderLookupFailed
	}
	if state, ok := p.byID[executionID]; ok {
		return state
	}
	return leaseHolderLookupFailed
}

func withLeaseProbe(t *testing.T, probe executionLivenessProbe, jobName string) {
	t.Helper()
	oldProbe := cloudLeaseProbe
	oldJob := cloudLeaseJobName
	cloudLeaseProbe = probe
	cloudLeaseJobName = func() string { return jobName }
	t.Cleanup(func() {
		cloudLeaseProbe = oldProbe
		cloudLeaseJobName = oldJob
	})
}

func seedLease(t *testing.T, m *memoryObjects, prefix, execution string) objectAttrs {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"execution":  execution,
		"started_at": "2026-08-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.Write(context.Background(), prefix+generation.LeasePath, payload, nil, objectConditions{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAllowLeaseReclaimPolicy(t *testing.T) {
	if allowLeaseReclaim(leaseHolderRunning) || allowLeaseReclaim(leaseHolderLookupFailed) || allowLeaseReclaim(leaseHolderMalformed) {
		t.Fatal("live/uncertain holders must not reclaim")
	}
	if !allowLeaseReclaim(leaseHolderTerminal) || !allowLeaseReclaim(leaseHolderNotFound) {
		t.Fatal("terminal/not_found holders must reclaim")
	}
}

func TestExecutionBelongsToJob(t *testing.T) {
	if !executionBelongsToJob("olw-pipeline-dev-bwqhz", "olw-pipeline-dev") {
		t.Fatal("expected job-prefixed execution to belong")
	}
	if executionBelongsToJob("olw-pipeline-dev", "olw-pipeline-dev") {
		t.Fatal("bare job name is not an execution id")
	}
	if executionBelongsToJob("olw-pipeline-other-abc", "olw-pipeline-dev") {
		t.Fatal("foreign job prefix must not belong")
	}
	if executionBelongsToJob("evil", "olw-pipeline-dev") {
		t.Fatal("unrelated id must not belong")
	}
	if executionBelongsToJob("olw-pipeline-dev-bwqhz", "") {
		t.Fatal("empty job must not match")
	}
}

// TestLWC222AcceptanceHappyPathOrphanHistoryReclaim is the LWC-222 acceptance
// gate (unit, not live orphan pipeline):
//
//	existing lease.json held by a Cloud Run history execution that is no longer
//	RUNNING → CAS reclaim → create-only acquire succeeds for the challenger.
//
// Holder ids are real DEV history names (gcloud run jobs executions list
// olw-pipeline-dev, 2026-08-01). Probe responses are the v2 Admin API shapes
// observed for those completed executions (no live network).
func TestLWC222AcceptanceHappyPathOrphanHistoryReclaim(t *testing.T) {
	const (
		jobName    = "olw-pipeline-dev"
		// From DEV history: Completed, failedCount=1 (exit 1). Not RUNNING.
		historyID  = "olw-pipeline-dev-bwqhz"
		challenger = "olw-pipeline-dev-acceptance-challenger"
	)
	// Sanitized v2 GET executions/{id} body for olw-pipeline-dev-bwqhz
	// (completion via conditions + failedCount; no completionStatus field).
	historyBody := []byte(`{
		"name": "projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline-dev/executions/olw-pipeline-dev-bwqhz",
		"failedCount": 1,
		"conditions": [
			{"type": "Started", "state": "CONDITION_SUCCEEDED"},
			{"type": "Completed", "state": "CONDITION_FAILED", "executionReason": "NON_ZERO_EXIT_CODE"}
		]
	}`)

	m := newMemoryObjects()
	seedLease(t, m, "p/", historyID)

	probe := &cloudRunExecutionProbe{
		project:  "llm-wiki-cloud",
		location: "asia-east1",
		getToken: func(context.Context) (string, error) { return "tok", nil },
		get: func(_ context.Context, rawURL, token string) (int, []byte, error) {
			if token != "tok" {
				t.Fatalf("token=%q", token)
			}
			if !strings.Contains(rawURL, "/jobs/"+jobName+"/executions/"+historyID) {
				t.Fatalf("probe url=%s, want history holder %s", rawURL, historyID)
			}
			return 200, historyBody, nil
		},
	}
	withLeaseProbe(t, probe, jobName)

	if got := classifyCloudRunExecutionJSON(historyBody); got != leaseHolderTerminal {
		t.Fatalf("history body classified %s, want terminal (acceptance precondition)", got)
	}

	lease, err := acquireCloudLease(context.Background(), m, "p/", challenger)
	if err != nil {
		t.Fatalf("happy path reclaim/acquire: %v", err)
	}
	if lease == nil || lease.generation <= 0 {
		t.Fatal("expected lease after reclaim")
	}
	payload, _, err := m.Read(context.Background(), "p/"+generation.LeasePath, 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	var body leasePayload
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.Execution != challenger {
		t.Fatalf("lease holder=%q, want challenger %q (orphan history reclaimed)", body.Execution, challenger)
	}
}

func TestLWC222AcceptanceGateWhileHistoryWouldBeRunning(t *testing.T) {
	// Control case for the same acceptance suite: if probe says RUNNING, gate.
	const historyID = "olw-pipeline-dev-lplb8" // real DEV history name used only as id
	m := newMemoryObjects()
	seedLease(t, m, "p/", historyID)
	withLeaseProbe(t, &fixedLivenessProbe{state: leaseHolderRunning}, "olw-pipeline-dev")

	_, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-acceptance-blocked")
	if err == nil || !errors.Is(err, errCloudLeaseHeld) {
		t.Fatalf("RUNNING holder must gate, err=%v", err)
	}
	payload, _, err := m.Read(context.Background(), "p/"+generation.LeasePath, 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	var body leasePayload
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.Execution != historyID {
		t.Fatalf("gated lease must keep holder %q, got %q", historyID, body.Execution)
	}
}

func TestAcquireCloudLeaseReclaimsTerminalHolder(t *testing.T) {
	m := newMemoryObjects()
	seedLease(t, m, "p/", "olw-pipeline-dev-old1")
	probe := &fixedLivenessProbe{state: leaseHolderTerminal}
	withLeaseProbe(t, probe, "olw-pipeline-dev")

	lease, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-new1")
	if err != nil {
		t.Fatalf("acquire after terminal reclaim: %v", err)
	}
	if lease == nil || lease.generation <= 0 {
		t.Fatal("expected lease")
	}
	if probe.calls.Load() != 1 {
		t.Fatalf("probe calls=%d, want 1", probe.calls.Load())
	}
	payload, _, err := m.Read(context.Background(), "p/"+generation.LeasePath, 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	var body leasePayload
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.Execution != "olw-pipeline-dev-new1" {
		t.Fatalf("lease holder=%q, want new execution", body.Execution)
	}
}

func TestAcquireCloudLeaseReclaimsNotFoundHolder(t *testing.T) {
	m := newMemoryObjects()
	seedLease(t, m, "p/", "olw-pipeline-dev-gone")
	withLeaseProbe(t, &fixedLivenessProbe{state: leaseHolderNotFound}, "olw-pipeline-dev")

	if _, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-next"); err != nil {
		t.Fatalf("not_found reclaim failed: %v", err)
	}
}

func TestAcquireCloudLeaseRefusesRunningHolder(t *testing.T) {
	m := newMemoryObjects()
	seedLease(t, m, "p/", "olw-pipeline-dev-live")
	withLeaseProbe(t, &fixedLivenessProbe{state: leaseHolderRunning}, "olw-pipeline-dev")

	_, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-challenger")
	if err == nil {
		t.Fatal("expected block while holder RUNNING")
	}
	if !errors.Is(err, errCloudLeaseHeld) {
		t.Fatalf("error=%v, want errCloudLeaseHeld", err)
	}
	if !strings.Contains(err.Error(), "pipeline publish lease is held") {
		t.Fatalf("public message missing: %v", err)
	}
	// Unwrap chain should surface holder state without secrets.
	if !strings.Contains(err.Error(), "pipeline publish lease is held") {
		t.Fatal(err)
	}
	cause := err
	for cause != nil {
		if strings.Contains(cause.Error(), "holder_state=RUNNING") && strings.Contains(cause.Error(), "olw-pipeline-dev-live") {
			return
		}
		cause = errors.Unwrap(cause)
	}
	// annotatedError uses Is on public; walk via Unwrap
	type unwrapper interface{ Unwrap() error }
	var walk error = err
	found := false
	for walk != nil {
		if strings.Contains(walk.Error(), "holder_state=RUNNING") {
			found = true
			break
		}
		u, ok := walk.(unwrapper)
		if !ok {
			break
		}
		walk = u.Unwrap()
	}
	if !found {
		// detail is in cause of annotatedError — Error() returns public only.
		// errors.Is still works for public; root is available via Unwrap.
		if ae, ok := err.(*annotatedError); ok {
			if ae.cause == nil || !strings.Contains(ae.cause.Error(), "holder_state=RUNNING") {
				t.Fatalf("missing holder detail in cause: %#v", ae.cause)
			}
		} else {
			t.Fatalf("want annotatedError with holder detail, got %T %v", err, err)
		}
	}
}

func TestAcquireCloudLeaseRefusesLookupFailed(t *testing.T) {
	m := newMemoryObjects()
	seedLease(t, m, "p/", "olw-pipeline-dev-old")
	withLeaseProbe(t, &fixedLivenessProbe{state: leaseHolderLookupFailed}, "olw-pipeline-dev")

	_, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-new")
	if err == nil || !errors.Is(err, errCloudLeaseHeld) {
		t.Fatalf("lookup_failed must fail closed: %v", err)
	}
	// Lease object must remain for the original holder.
	payload, _, err := m.Read(context.Background(), "p/"+generation.LeasePath, 0, generation.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	var body leasePayload
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.Execution != "olw-pipeline-dev-old" {
		t.Fatalf("lease mutated on lookup_failed: %v", body)
	}
}

func TestAcquireCloudLeaseRefusesAgeBasedWithoutProbeTerminal(t *testing.T) {
	// Regression: started_at age alone must never reclaim. Probe stays RUNNING
	// even if started_at is ancient.
	m := newMemoryObjects()
	payload, _ := json.Marshal(map[string]string{
		"execution":  "olw-pipeline-dev-ancient",
		"started_at": "2020-01-01T00:00:00Z",
	})
	if _, err := m.Write(context.Background(), "p/"+generation.LeasePath, payload, nil, objectConditions{}); err != nil {
		t.Fatal(err)
	}
	withLeaseProbe(t, &fixedLivenessProbe{state: leaseHolderRunning}, "olw-pipeline-dev")
	if _, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-new"); err == nil {
		t.Fatal("age must not enable reclaim while RUNNING")
	}
}

func TestAcquireCloudLeaseRefusesForeignJobPrefix(t *testing.T) {
	m := newMemoryObjects()
	seedLease(t, m, "p/", "other-job-exec1")
	// Even if probe would say terminal, foreign prefix is malformed for this job.
	withLeaseProbe(t, &fixedLivenessProbe{state: leaseHolderTerminal}, "olw-pipeline-dev")

	_, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-new")
	if err == nil {
		t.Fatal("foreign holder must not reclaim")
	}
	payload, _, _ := m.Read(context.Background(), "p/"+generation.LeasePath, 0, generation.MaxFileBytes)
	var body leasePayload
	_ = json.Unmarshal(payload, &body)
	if body.Execution != "other-job-exec1" {
		t.Fatalf("lease should remain foreign holder, got %v", body)
	}
}

func TestAcquireCloudLeaseConcurrentReclaimOnlyOneWins(t *testing.T) {
	m := newMemoryObjects()
	seedLease(t, m, "p/", "olw-pipeline-dev-dead")
	// Only the orphaned holder is terminal. Any winner's new lease must look
	// RUNNING so later racers cannot steal a live holder's lease.
	withLeaseProbe(t, &selectiveLivenessProbe{
		terminal: map[string]struct{}{"olw-pipeline-dev-dead": {}},
	}, "olw-pipeline-dev")
	const n = 8
	var wg sync.WaitGroup
	var wins atomic.Int32
	var blocks atomic.Int32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("olw-pipeline-dev-racer-%d", i)
			lease, err := acquireCloudLease(context.Background(), m, "p/", id)
			if err == nil && lease != nil {
				wins.Add(1)
				return
			}
			if err != nil {
				blocks.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("wins=%d, want exactly 1 concurrent reclaim winner", wins.Load())
	}
	if wins.Load()+blocks.Load() != n {
		t.Fatalf("wins+blocks=%d, want %d", wins.Load()+blocks.Load(), n)
	}
}

// selectiveLivenessProbe marks listed ids terminal; every other id is RUNNING.
type selectiveLivenessProbe struct {
	terminal map[string]struct{}
}

func (p *selectiveLivenessProbe) Probe(_ context.Context, executionID, _ string) leaseHolderLiveness {
	if _, ok := p.terminal[executionID]; ok {
		return leaseHolderTerminal
	}
	return leaseHolderRunning
}

func TestClassifyCloudRunExecutionJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
		want leaseHolderLiveness
	}{
		{name: "completion succeeded", body: `{"completionStatus":"EXECUTION_SUCCEEDED"}`, want: leaseHolderTerminal},
		{name: "completion failed", body: `{"completionStatus":"EXECUTION_FAILED"}`, want: leaseHolderTerminal},
		{name: "completion cancelled", body: `{"completionStatus":"EXECUTION_CANCELLED"}`, want: leaseHolderTerminal},
		// Real DEV history (olw-pipeline-dev-bwqhz): v2 often has no completionStatus.
		{name: "history completed failedCount+condition", body: `{"failedCount":1,"conditions":[{"type":"Completed","state":"CONDITION_FAILED"}]}`, want: leaseHolderTerminal},
		{name: "reconciling", body: `{"reconciling":true}`, want: leaseHolderRunning},
		{name: "running count", body: `{"runningCount":1}`, want: leaseHolderRunning},
		// Multi-task partial progress: counters must not win over live signals.
		{name: "partial failed still running", body: `{"failedCount":1,"runningCount":1}`, want: leaseHolderRunning},
		{name: "partial succeeded still reconciling", body: `{"succeededCount":1,"reconciling":true}`, want: leaseHolderRunning},
		{name: "empty unknown", body: `{}`, want: leaseHolderLookupFailed},
		{name: "garbage", body: `not-json`, want: leaseHolderLookupFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCloudRunExecutionJSON([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestCloudRunExecutionProbeHTTP(t *testing.T) {
	probe := &cloudRunExecutionProbe{
		project:  "llm-wiki-cloud",
		location: "asia-east1",
		getToken: func(context.Context) (string, error) { return "tok", nil },
		get: func(_ context.Context, rawURL, token string) (int, []byte, error) {
			if token != "tok" {
				t.Fatalf("token=%q", token)
			}
			if !strings.Contains(rawURL, "/jobs/olw-pipeline-dev/executions/olw-pipeline-dev-abc") {
				t.Fatalf("url=%s", rawURL)
			}
			return 200, []byte(`{"completionStatus":"EXECUTION_FAILED"}`), nil
		},
	}
	if got := probe.Probe(context.Background(), "olw-pipeline-dev-abc", "olw-pipeline-dev"); got != leaseHolderTerminal {
		t.Fatalf("got %s", got)
	}

	probe.get = func(context.Context, string, string) (int, []byte, error) {
		return 404, nil, nil
	}
	if got := probe.Probe(context.Background(), "olw-pipeline-dev-missing", "olw-pipeline-dev"); got != leaseHolderNotFound {
		t.Fatalf("404 got %s", got)
	}

	probe.get = func(context.Context, string, string) (int, []byte, error) {
		return 403, nil, nil
	}
	if got := probe.Probe(context.Background(), "olw-pipeline-dev-abc", "olw-pipeline-dev"); got != leaseHolderLookupFailed {
		t.Fatalf("403 got %s", got)
	}
}

func TestParsePipelineJobURLScope(t *testing.T) {
	p, l, ok := parsePipelineJobURLScope("https://run.googleapis.com/v2/projects/p1/locations/asia-east1/jobs/olw-pipeline-dev:run")
	if !ok || p != "p1" || l != "asia-east1" {
		t.Fatalf("got %q %q %v", p, l, ok)
	}
	if _, _, ok := parsePipelineJobURLScope("https://evil.example/v2/projects/p/locations/r/jobs/j:run"); ok {
		t.Fatal("foreign host must fail")
	}
}

func TestExistingOverlapTestStillBlocksWithoutProbeTerminal(t *testing.T) {
	// Preserve historical semantics when probe refuses (default path in this test
	// uses a RUNNING probe so second acquire still fails).
	m := newMemoryObjects()
	withLeaseProbe(t, &fixedLivenessProbe{state: leaseHolderRunning}, "olw-pipeline-dev")
	first, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-exec-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-exec-y"); err == nil {
		t.Fatal("second lease succeeded while holder running")
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireCloudLease(context.Background(), m, "p/", "olw-pipeline-dev-exec-z"); err != nil {
		t.Fatal(err)
	}
}
