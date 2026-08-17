package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReceiptRecordsDistinctStagesCallsAndRedactsSensitiveValues(t *testing.T) {
	ctx, recorder := WithReceipt(context.Background())
	expansion := recorder.StartStage(ctx, "query_expansion", "deepseek", "deepseek-v4-flash", "none")
	finishCall := recorder.StartCall("query_expansion", "deepseek-v4-flash", "none")
	time.Sleep(time.Millisecond)
	finishCall("provider_error")
	FinishStage(expansion, "fallback")
	local := recorder.StartStage(ctx, "candidate_matching", "", "", "")
	FinishStage(local, "success")
	FinishReceipt(recorder)

	got := recorder.Receipt()
	if len(got.Stages) != 2 || len(got.HostCalls) != 1 || got.HostCalls[0].Sequence != 1 {
		t.Fatalf("receipt = %#v", got)
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
