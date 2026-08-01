package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/generation"
)

// leaseHolderLiveness is the resolved Cloud Run execution state of a lease holder.
// Age/TTL is never a reclaim signal (LWC-222 / LWC-170).
type leaseHolderLiveness string

const (
	leaseHolderRunning      leaseHolderLiveness = "RUNNING"
	leaseHolderTerminal     leaseHolderLiveness = "terminal"
	leaseHolderNotFound     leaseHolderLiveness = "not_found"
	leaseHolderLookupFailed leaseHolderLiveness = "lookup_failed"
	leaseHolderMalformed    leaseHolderLiveness = "malformed"
)

// leasePayload is the create-only GCS lease object body.
type leasePayload struct {
	Execution string `json:"execution"`
	StartedAt string `json:"started_at"`
}

// executionLivenessProbe resolves whether a Cloud Run Jobs execution is still live.
// Implementations must fail closed on uncertainty (never invent terminal).
type executionLivenessProbe interface {
	// Probe returns a finite liveness classification for the holder execution id.
	// jobName is the expected Cloud Run Job name (e.g. olw-pipeline-dev). Empty
	// jobName forces lookup_failed so reclaim cannot proceed without scoping.
	Probe(ctx context.Context, executionID, jobName string) leaseHolderLiveness
}

// cloudLeaseProbe is injectable; production uses cloudRunExecutionProbe.
var cloudLeaseProbe executionLivenessProbe = newCloudRunExecutionProbe()

// cloudLeaseJobName resolves the expected job name for ownership checks.
var cloudLeaseJobName = func() string {
	for _, key := range []string{"CLOUD_RUN_JOB", "PIPELINE_JOB_NAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// allowLeaseReclaim reports whether a probed holder state may be CAS-reclaimed.
// RUNNING, lookup failures, and malformed holders never reclaim.
// Terminal and proven not-found (well-formed + job-scoped) may reclaim.
func allowLeaseReclaim(state leaseHolderLiveness) bool {
	switch state {
	case leaseHolderTerminal, leaseHolderNotFound:
		return true
	default:
		return false
	}
}

func parseLeasePayload(data []byte) (leasePayload, error) {
	var payload leasePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return leasePayload{}, err
	}
	payload.Execution = strings.TrimSpace(payload.Execution)
	payload.StartedAt = strings.TrimSpace(payload.StartedAt)
	if payload.Execution == "" || !validPipelineExecutionID(payload.Execution) {
		return leasePayload{}, errors.New("lease payload missing valid execution id")
	}
	return payload, nil
}

// executionBelongsToJob enforces that holder ids look like executions of the
// configured job (Cloud Run names are "{job}-{suffix}"). Foreign or bare ids
// refuse reclaim even if the API returns not found.
func executionBelongsToJob(executionID, jobName string) bool {
	executionID = strings.TrimSpace(executionID)
	jobName = strings.TrimSpace(jobName)
	if executionID == "" || jobName == "" {
		return false
	}
	if executionID == jobName {
		return false
	}
	return strings.HasPrefix(executionID, jobName+"-")
}

// acquireCloudLease creates a create-only GCS lease. On DoesNotExist conflict it
// probes the holder execution via Cloud Run; terminal/not-found holders are
// reclaimed once with generation-matched delete + create-only rewrite (LWC-222).
func acquireCloudLease(ctx context.Context, store objectStore, prefix, execution string) (*cloudLease, error) {
	holder := strings.TrimSpace(execution)
	if !validPipelineExecutionID(holder) {
		return nil, errors.New("invalid lease execution id")
	}
	leaseName := prefix + generation.LeasePath

	lease, err := tryCreateCloudLease(ctx, store, leaseName, holder)
	if err == nil {
		return lease, nil
	}
	if !errors.Is(err, errObjectGenerationConflict) {
		return nil, annotateError(errCloudLeaseHeld, err)
	}

	// Conflict: inspect holder liveness before fail-closed.
	reclaimed, detail, reclaimErr := maybeReclaimCloudLease(ctx, store, leaseName, holder)
	if reclaimErr != nil {
		return nil, annotateError(errCloudLeaseHeld, reclaimErr)
	}
	if !reclaimed {
		return nil, annotateError(errCloudLeaseHeld, detail)
	}

	// Exactly one create-only retry after successful CAS reclaim.
	lease, err = tryCreateCloudLease(ctx, store, leaseName, holder)
	if err != nil {
		return nil, annotateError(errCloudLeaseHeld, fmt.Errorf("lease reclaim succeeded but re-acquire failed: %w", err))
	}
	return lease, nil
}

func tryCreateCloudLease(ctx context.Context, store objectStore, leaseName, holder string) (*cloudLease, error) {
	payload, err := json.Marshal(map[string]string{
		"execution":  holder,
		"started_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return nil, err
	}
	a, err := store.Write(ctx, leaseName, payload, nil, objectConditions{DoesNotExist: true})
	if err != nil {
		return nil, err
	}
	return &cloudLease{store: store, name: leaseName, generation: a.Generation}, nil
}

// maybeReclaimCloudLease returns reclaimed=true only after a successful
// generation-matched delete of an abandoned lease. detail is a non-secret
// wrapped cause suitable for annotateError when reclaimed=false.
func maybeReclaimCloudLease(ctx context.Context, store objectStore, leaseName, requester string) (reclaimed bool, detail error, err error) {
	data, attrs, readErr := store.Read(ctx, leaseName, 0, generation.MaxFileBytes)
	if readErr != nil {
		if isObjectNotFound(readErr) {
			// Lost the race: holder released between conflict and read. Caller retries create.
			return true, nil, nil
		}
		return false, nil, fmt.Errorf("read existing lease: %w", readErr)
	}
	if attrs.Generation <= 0 {
		return false, fmt.Errorf("existing lease generation unavailable"), nil
	}

	payload, parseErr := parseLeasePayload(data)
	if parseErr != nil {
		log.Printf("worker: lease reclaim refused state=%s requester=%s reason=malformed_payload", leaseHolderMalformed, requester)
		// Preserve generation-conflict root so callers can still errors.Is conflict.
		return false, fmt.Errorf("%w: holder_state=%s: %v", errObjectGenerationConflict, leaseHolderMalformed, parseErr), nil
	}

	jobName := strings.TrimSpace(cloudLeaseJobName())
	if jobName == "" || !executionBelongsToJob(payload.Execution, jobName) {
		log.Printf("worker: lease reclaim refused state=%s holder=%s job=%q", leaseHolderMalformed, payload.Execution, jobName)
		return false, fmt.Errorf("%w: holder_state=%s holder=%s started_at=%s", errObjectGenerationConflict, leaseHolderMalformed, payload.Execution, payload.StartedAt), nil
	}

	state := cloudLeaseProbe.Probe(ctx, payload.Execution, jobName)
	log.Printf("worker: lease conflict holder=%s started_at=%s state=%s reclaim=%v",
		payload.Execution, payload.StartedAt, state, allowLeaseReclaim(state))

	if !allowLeaseReclaim(state) {
		return false, fmt.Errorf("%w: holder_state=%s holder=%s started_at=%s", errObjectGenerationConflict, state, payload.Execution, payload.StartedAt), nil
	}

	if delErr := store.Delete(ctx, leaseName, attrs.Generation); delErr != nil {
		if errors.Is(delErr, errObjectGenerationConflict) || isObjectNotFound(delErr) {
			// Concurrent reclaim/release won; treat as reclaimed so create-only can proceed or fail cleanly.
			return true, nil, nil
		}
		return false, nil, fmt.Errorf("CAS delete abandoned lease: %w", delErr)
	}
	return true, nil, nil
}
