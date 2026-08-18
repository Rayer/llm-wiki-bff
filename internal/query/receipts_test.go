package query

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReceiptRecordsDistinctStagesCallsAndRedactsSensitiveValues(t *testing.T) {
	ctx, recorder := WithReceipt(context.Background())
	recorder.SetRetrievalConfig(10, 1, 2)
	expansion := recorder.StartStage(ctx, "query_expansion", "deepseek", "deepseek-v4-flash", "none")
	finishCall := recorder.StartCallAtTimed("query_expansion", "deepseek-v4-flash", "none", "https://api.deepseek.com")
	finishCall("provider_error", time.Now().Add(time.Millisecond))
	FinishStage(expansion, "fallback")
	local := recorder.StartStage(ctx, "candidate_matching", "", "", "")
	FinishStage(local, "success")
	FinishReceipt(recorder)

	got := recorder.Receipt()
	if len(got.Stages) != 2 || len(got.HostCalls) != 1 || got.HostCalls[0].Sequence != 1 {
		t.Fatalf("receipt = %#v", got)
	}
	if got.SelectionLimit != 10 || got.ExplorationSlots != 1 || got.EvidenceThreshold != 2 {
		t.Fatalf("retrieval config receipt = %#v", got)
	}
	if got.HostCalls[0].Stage != "query_expansion" || got.HostCalls[0].Outcome != "provider_error" || got.HostCalls[0].Scheme != "https" || got.HostCalls[0].Host != "api.deepseek.com" {
		t.Fatalf("host call = %#v", got.HostCalls[0])
	}
	if got.HostCalls[0].ElapsedMS < 1 || got.Stages[0].FinishedAt.IsZero() || got.Stages[1].FinishedAt.IsZero() {
		t.Fatalf("timing was not closed: %#v", got)
	}
	data, _ := json.Marshal(got)
	for _, forbidden := range []string{"raw-query", "api-key", "/chat/completions", "Authorization", "prompt-body", "provider-request-id"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("receipt leaked %q: %s", forbidden, data)
		}
	}
}

func TestReceiptTimesAreRFC3339NanoUTCAndHostIdentityIsNormalized(t *testing.T) {
	_, recorder := WithReceipt(context.Background())
	finish := recorder.StartCallAt("answer_synthesis", "configured-model", "low", "http://127.0.0.1:8080/path?q=x")
	finish("success")
	FinishReceipt(recorder)
	got := recorder.Receipt()
	if got.QueryReceivedAt.Location() != time.UTC || got.RunStartedAt.Location() != time.UTC || got.RunFinishedAt.Location() != time.UTC {
		t.Fatalf("run timestamps are not UTC: %#v", got)
	}
	if len(got.HostCalls) != 0 {
		t.Fatalf("IP host was persisted: %#v", got.HostCalls)
	}
	data, _ := json.Marshal(got)
	for _, field := range []string{"query_received_at", "run_started_at", "run_finished_at"} {
		if !strings.Contains(string(data), `"`+field+`":"`) || !strings.Contains(string(data), "Z") {
			t.Fatalf("timestamp %s is not RFC3339 UTC: %s", field, data)
		}
	}
	for _, forbidden := range []string{"127.0.0.1", "::1", "/path", "q=x", "configured-credential"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("receipt leaked %q: %s", forbidden, data)
		}
	}
}

func TestTimedHostReceiptEndsAtNetworkBoundary(t *testing.T) {
	_, recorder := WithReceipt(context.Background())
	started := time.Now()
	finish := recorder.StartCallAtTimed("answer_synthesis", "configured-model", "low", "https://api.deepseek.com/chat/completions")
	networkFinished := started.Add(10 * time.Millisecond)
	finish("decode_error", networkFinished)
	got := recorder.Receipt()
	if len(got.HostCalls) != 1 || got.HostCalls[0].Outcome != "decode_error" || !got.HostCalls[0].FinishedAt.Equal(networkFinished.UTC()) {
		t.Fatalf("timed host receipt = %#v", got.HostCalls)
	}
}

func TestReceiptRecordsResolvedExpansionConfigAndSupport(t *testing.T) {
	_, recorder := WithReceipt(context.Background())
	recorder.SetExpansionConfig(3, 2, 1, 24, 2, 1, 2, []KeywordSupportReceipt{{Role: "preferred", Kind: "sentinel-value", Value: "raw-query", Keyword: "provider-body", SupportCount: 2, AttemptIndexes: []int{1, 2}}})
	got := recorder.Receipt()
	if got.ExpansionAttempts != 3 || got.SuccessfulExpansionAttempts != 2 || got.ProviderFailedExpansionAttempts != 1 || got.FallbackExpansionCount != 0 || got.KeywordsPerExpansionAttempt != 24 || got.EvidenceThreshold != 2 || got.KeywordConsensusMinimum != 2 || got.RareKeywordMaxDocumentFrequency != 1 {
		t.Fatalf("expansion config receipt = %#v", got)
	}
	if len(got.KeywordSupport) != 1 || got.KeywordSupport[0].SupportCount != 2 || !reflect.DeepEqual(got.KeywordSupport[0].AttemptIndexes, []int{1, 2}) {
		t.Fatalf("keyword support receipt = %#v", got.KeywordSupport)
	}
	data, _ := json.Marshal(got)
	encoded := string(data)
	for _, forbidden := range []string{"coffee", "sentinel-value", "raw-query", "provider-body"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("receipt leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(encoded, "keyword_digest") || !strings.Contains(encoded, "support_count") || !strings.Contains(encoded, "attempt_indexes") {
		t.Fatalf("receipt lost auditable sanitized support: %s", encoded)
	}
}

func TestReceiptDigestsSurfaceFormsWithoutPersistingThem(t *testing.T) {
	_, recorder := WithReceipt(context.Background())
	recorder.SetExpansionConfig(1, 1, 0, 1, 2, 1, 2, []KeywordSupportReceipt{{Role: "preferred", Kind: "concept", Value: "value", Keyword: "alias", SurfaceForms: []string{"surface-form"}, SupportCount: 1, AttemptIndexes: []int{1}}})
	data, _ := json.Marshal(recorder.Receipt())
	encoded := string(data)
	if strings.Contains(encoded, "surface-form") || !strings.Contains(encoded, "surface_form_digests") {
		t.Fatalf("surface form was not sanitized: %s", encoded)
	}
}
