package queryquality_test

import (
	"context"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

type profileExpander struct{ policy queryquality.CriterionPolicy }

func (e *profileExpander) Expand(_ context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	e.policy = request.CriterionPolicy
	return queryquality.QueryPlan{RawQuery: request.Query, Preferred: []queryquality.Criterion{{Kind: "topic", Value: "x", Terms: []string{"x"}, Proof: "lexical"}}}, nil
}

type profileMatcher struct{}

func (profileMatcher) Match(context.Context, queryquality.MatchRequest) (queryquality.EligibilityResult, error) {
	return queryquality.EligibilityResult{}, nil
}

type profileSelector struct{}

func (profileSelector) Select(context.Context, queryquality.SelectionInput) (queryquality.SelectionResult, error) {
	return queryquality.SelectionResult{}, nil
}

type profileReader struct{}

func (profileReader) Read(context.Context, func(cache.Entry) error) error        { return nil }
func (profileReader) Prefix() string                                             { return "test" }
func (profileReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) { return nil, nil }
func (profileReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, nil
}

func TestPipelineUsesValidatedCopiedProfile(t *testing.T) {
	required := []string{"topic"}
	profile := queryquality.RetrievalProfile{ID: "experiment-v1", CriterionPolicy: queryquality.CriterionPolicy{RequiredWhenExplicit: required}}
	digest, err := profile.Digest()
	if err != nil {
		t.Fatal(err)
	}
	expander := &profileExpander{}
	pipeline, err := queryquality.NewQueryRetrievalPipelineWithProfile(expander, profileMatcher{}, profileSelector{}, nil, profile)
	if err != nil {
		t.Fatal(err)
	}
	required[0] = "mutated"
	ctx, recorder := query.WithReceipt(context.Background())
	if _, err := pipeline.Execute(ctx, profileReader{}, query.Request{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if got := expander.policy.RequiredWhenExplicit; len(got) != 1 || got[0] != "topic" {
		t.Fatalf("policy=%v", got)
	}
	if got := recorder.Receipt(); got.RetrievalProfileID != "experiment-v1" || got.RetrievalProfileDigest != digest {
		t.Fatalf("receipt profile=%+v", got)
	}
}

func TestInvalidProfileRejectedAtConstruction(t *testing.T) {
	_, err := queryquality.NewQueryRetrievalPipelineWithProfile(nil, nil, nil, nil, queryquality.RetrievalProfile{ID: "bad id"})
	if err == nil {
		t.Fatal("invalid profile accepted")
	}
}

func TestProfileDigestIsCanonicalAndPrivacySafe(t *testing.T) {
	left := queryquality.RetrievalProfile{ID: "p1", CriterionPolicy: queryquality.CriterionPolicy{RequiredWhenExplicit: []string{"b", "a"}}}
	right := queryquality.RetrievalProfile{ID: "p1", CriterionPolicy: queryquality.CriterionPolicy{RequiredWhenExplicit: []string{"a", "b"}}}
	leftDigest, err := left.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := right.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest == "" || leftDigest == rightDigest {
		t.Fatalf("digests=%q,%q", leftDigest, rightDigest)
	}
	copyOfLeft, err := left.ValidatedCopy()
	if err != nil {
		t.Fatal(err)
	}
	copyDigest, err := copyOfLeft.Digest()
	if err != nil || copyDigest != leftDigest {
		t.Fatalf("validated copy digest=%q err=%v, want %q", copyDigest, err, leftDigest)
	}
	if leftDigest == "secret query" {
		t.Fatal("digest leaked input")
	}
}

func TestPipelineClonesPolicyForEachExecution(t *testing.T) {
	expander := &mutatingProfileExpander{}
	pipeline, err := queryquality.NewQueryRetrievalPipelineWithProfile(expander, profileMatcher{}, profileSelector{}, nil, queryquality.RetrievalProfile{
		ID: "profile-v1", CriterionPolicy: queryquality.CriterionPolicy{RequiredWhenExplicit: []string{"topic"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pipeline.Execute(context.Background(), profileReader{}, query.Request{Query: "q"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(expander.policies) != 2 || len(expander.policies[1].RequiredWhenExplicit) != 1 || expander.policies[1].RequiredWhenExplicit[0] != "topic" {
		t.Fatalf("policies=%#v", expander.policies)
	}
}

type mutatingProfileExpander struct {
	policies []queryquality.CriterionPolicy
}

func (e *mutatingProfileExpander) Expand(_ context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	e.policies = append(e.policies, queryquality.CriterionPolicy{
		RequiredWhenExplicit: append([]string(nil), request.CriterionPolicy.RequiredWhenExplicit...),
		PreferredByDefault:   append([]string(nil), request.CriterionPolicy.PreferredByDefault...),
		GoalsToExpand:        append([]string(nil), request.CriterionPolicy.GoalsToExpand...),
	})
	request.CriterionPolicy.RequiredWhenExplicit[0] = "mutated"
	return queryquality.QueryPlan{RawQuery: request.Query, Preferred: []queryquality.Criterion{{Kind: "topic", Value: "x", Terms: []string{"x"}, Proof: "lexical"}}}, nil
}
