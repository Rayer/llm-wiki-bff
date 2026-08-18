package queryquality_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/query"
	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func TestCorePlanEligibilityAndSelectionContracts(t *testing.T) {
	plan, err := queryquality.DecodePlan(`{"raw_query":"Taipei cafe","required":[{"kind":"location","value":"Taipei","terms":["Taipei"],"proof":"lexical"}],"excluded":[{"kind":"exclusion","value":"smoking","terms":["smoking"],"proof":"lexical"}],"preferred":[{"kind":"venue_type","value":"cafe","terms":["cafe"],"proof":"lexical"}],"goals":[{"kind":"recommendation","value":"work","terms":["work"],"proof":"lexical"}],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`, "Taipei cafe")
	if err != nil {
		t.Fatal(err)
	}
	entries := []cache.Entry{
		{Slug: "positive", Title: "Taipei cafe", Body: "Taipei cafe for work"},
		{Slug: "optional-miss", Title: "Taipei place", Body: "Taipei"},
		{Slug: "required-miss", Title: "Kaohsiung cafe", Body: "cafe for work"},
		{Slug: "excluded", Title: "Taipei smoking cafe", Body: "Taipei cafe smoking"},
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: entries})
	if err != nil {
		t.Fatal(err)
	}
	bySlug := make(map[string]queryquality.CandidateEvidence, len(matched.Candidates))
	for _, candidate := range matched.Candidates {
		bySlug[candidate.Slug] = candidate
	}
	if !bySlug["positive"].Eligible || !bySlug["optional-miss"].Eligible {
		t.Fatal("optional misses must not hard-gate an eligible candidate")
	}
	if bySlug["required-miss"].Eligible || bySlug["excluded"].Eligible {
		t.Fatal("required/excluded criteria were not enforced conservatively")
	}

	semanticPlan := queryquality.QueryPlan{RawQuery: "semantic", Required: []queryquality.Criterion{{Kind: "activity", Value: "skiing", Terms: []string{"skiing"}, Proof: "semantic"}}}
	semanticMatched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: semanticPlan, CorpusEntries: []cache.Entry{{Slug: "lexical", Title: "Lexical", Body: "skiing"}}})
	if err != nil {
		t.Fatal(err)
	}
	if semanticMatched.Candidates[0].Eligible || semanticMatched.Candidates[0].Groups[0].SemanticOutcome != "unresolved" {
		t.Fatal("lexical text must not infer semantic proof")
	}

	candidates := append(matched.Candidates, queryquality.CandidateEvidence{Slug: "ineligible-high-score", Title: "bad", Eligible: false, Score: 99})
	selector := queryquality.NewResultSelector()
	input := queryquality.SelectionInput{Candidates: candidates, Limit: 2, ExplorationSlots: 1, Seed: 42}
	first, err := selector.Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := selector.Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("selection replay differs: %s vs %s", firstJSON, secondJSON)
	}
	for _, candidate := range first.Selected {
		if candidate.Selected && candidate.Slug == "ineligible-high-score" {
			t.Fatal("exploration admitted an ineligible candidate")
		}
	}
	if !containsSelected(first.Selected, "positive") {
		t.Fatal("known-positive fixture was not selected")
	}
}

func TestEvidenceThresholdQualificationAndSelection(t *testing.T) {
	plan := queryquality.QueryPlan{Preferred: []queryquality.Criterion{{Kind: "venue_type", Value: "cafe", Terms: []string{"cafe"}, Proof: "lexical"}}}
	entries := []cache.Entry{
		{Slug: "no-evidence", Title: "No evidence", Body: "library"},
		{Slug: "one", Title: "One cafe", Body: "cafe"},
		{Slug: "two", Title: "Two cafe", Body: "cafe"},
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: entries, EvidenceThreshold: 1, EvidenceThresholdSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bySlug := make(map[string]queryquality.CandidateEvidence, len(matched.Candidates))
	for _, candidate := range matched.Candidates {
		bySlug[candidate.Slug] = candidate
	}
	if bySlug["no-evidence"].Qualified || bySlug["no-evidence"].Score != 0 {
		t.Fatalf("score-zero candidate = %#v, want unqualified", bySlug["no-evidence"])
	}
	selection, err := queryquality.NewResultSelector().Select(context.Background(), queryquality.SelectionInput{Candidates: matched.Candidates, Limit: 10, ExplorationSlots: 1, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	for _, decision := range selection.Selected {
		if decision.Selected {
			selected++
		}
	}
	if selected != 2 {
		t.Fatalf("selected count = %d, want 2 qualified candidates", selected)
	}

	legacy, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "legacy", Title: "Legacy", Body: "library"}}, EvidenceThreshold: 0, EvidenceThresholdSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.Candidates[0].Qualified || legacy.Candidates[0].QualificationReason != "legacy_threshold_zero" {
		t.Fatalf("threshold-zero candidate = %#v, want explicit legacy qualification", legacy.Candidates[0])
	}
}

func TestExactRequiredEntityQualifiesWithoutLegacyScore(t *testing.T) {
	for _, kind := range []string{"entity", "name", "entity_name", "entity-name", "venue_name", "venue-name"} {
		t.Run(kind, func(t *testing.T) {
			plan := queryquality.QueryPlan{Required: []queryquality.Criterion{{Kind: kind, Value: "Boven", Terms: []string{"Boven"}, Proof: "lexical"}}}
			matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
				Plan: plan, CorpusEntries: []cache.Entry{{Slug: "boven", Title: "Boven 雜誌圖書館"}}, EvidenceThreshold: 3, EvidenceThresholdSet: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			candidate := matched.Candidates[0]
			if !candidate.ExactIdentityEvidence || !candidate.Qualified || candidate.Score != 0 {
				t.Fatalf("candidate = %#v, want exact qualified score zero", candidate)
			}
		})
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan:          queryquality.QueryPlan{Required: []queryquality.Criterion{{Kind: "entity", Value: "Boven", Terms: []string{"Boven"}, Proof: "lexical"}}},
		CorpusEntries: []cache.Entry{{Slug: "body-only", Title: "雜誌圖書館", Body: "Boven"}}, EvidenceThreshold: 1, EvidenceThresholdSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := matched.Candidates[0]
	if candidate.ExactIdentityEvidence || candidate.Qualified || candidate.Score != 0 {
		t.Fatalf("body-only candidate = %#v, want no exact identity and score-zero rejection", candidate)
	}
}

func TestCriterionEvidenceIsRoleAndTermLocal(t *testing.T) {
	plan := queryquality.QueryPlan{
		Required:  []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee shop"}, Proof: "lexical"}},
		Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}},
		Excluded:  []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"tea"}, Proof: "lexical"}},
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "preferred-only", Title: "Coffee"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := matched.Candidates[0]
	if candidate.Eligible || candidate.Rejection != "required_topic_not_matched" {
		t.Fatalf("candidate = %#v, want required miss despite preferred match", candidate)
	}

	plan = queryquality.QueryPlan{
		Required:  []queryquality.Criterion{{Kind: "entity", Value: "Boven", Terms: []string{"Boven"}, Proof: "lexical"}},
		Preferred: []queryquality.Criterion{{Kind: "entity", Value: "Boven", Terms: []string{"Boven"}, Proof: "lexical"}},
	}
	matched, err = queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "body-only", Title: "Other", Body: "Boven"}}})
	if err != nil {
		t.Fatal(err)
	}
	if matched.Candidates[0].ExactIdentityEvidence {
		t.Fatal("preferred/body evidence must not establish required exact identity")
	}

	plan = queryquality.QueryPlan{
		Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}},
		Excluded:  []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}},
	}
	matched, err = queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "excluded", Title: "Coffee"}}})
	if err != nil {
		t.Fatal(err)
	}
	if matched.Candidates[0].Eligible || matched.Candidates[0].Rejection != "excluded_criterion_matched" {
		t.Fatalf("candidate = %#v, want excluded role to reject independently", matched.Candidates[0])
	}
}

func TestPositiveEvidenceDimensionsCountPrefersRoleLocalMatches(t *testing.T) {
	plan := queryquality.QueryPlan{
		Preferred:              []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"cafe"}, Proof: "lexical"}},
		SupportingDimensions:   []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}},
		Goals:                  []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"espresso"}, Proof: "lexical"}},
		AcceptableAlternatives: []queryquality.Criterion{{Kind: "topic", Value: "tea", Terms: []string{"tea"}, Proof: "lexical"}},
	}
	matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{
		Plan:              plan,
		CorpusEntries:     []cache.Entry{{Slug: "coffee", Title: "Coffee", Body: "coffee"}},
		EvidenceThreshold: 1, EvidenceThresholdSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := matched.Candidates[0]
	if candidate.PositiveEvidenceCount != 1 {
		t.Fatalf("candidate = %#v, want positive-evidence-count 1", candidate)
	}
	if len(candidate.PositiveEvidenceDimensions) != 1 || candidate.PositiveEvidenceDimensions[0] != "topic" {
		t.Fatalf("candidate = %#v, want one normalized topic dimension", candidate)
	}
	if !candidate.Qualified {
		t.Fatalf("candidate = %#v, want threshold to qualify", candidate)
	}
}

func TestSelectorPreservesHardIneligibilityRejection(t *testing.T) {
	result, err := queryquality.NewResultSelector().Select(context.Background(), queryquality.SelectionInput{Candidates: []queryquality.CandidateEvidence{
		{Slug: "required-miss", Eligible: false, Rejection: "required_location_not_matched"},
		{Slug: "excluded", Eligible: false, Rejection: "excluded_criterion_matched"},
	}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range result.Selected {
		if decision.Reason != map[string]string{"required-miss": "required_location_not_matched", "excluded": "excluded_criterion_matched"}[decision.Slug] {
			t.Fatalf("decision = %#v, want preserved rejection", decision)
		}
	}
}

func TestEvidenceThresholdsChangeQualificationCausally(t *testing.T) {
	plan := queryquality.QueryPlan{Preferred: []queryquality.Criterion{
		{Kind: "venue_type", Value: "cafe", Terms: []string{"cafe"}, Proof: "lexical"},
		{Kind: "activity", Value: "work", Terms: []string{"work"}, Proof: "lexical"},
		{Kind: "setting", Value: "quiet", Terms: []string{"quiet"}, Proof: "lexical"},
	}}
	entry := cache.Entry{Slug: "candidate", Title: "Cafe", Body: "cafe work"}
	for _, test := range []struct {
		threshold int
		qualified bool
		count     int
	}{
		{threshold: 0, qualified: true, count: 2},
		{threshold: 1, qualified: true, count: 2},
		{threshold: 2, qualified: true, count: 2},
		{threshold: 3, qualified: false, count: 2},
		{threshold: 4, qualified: false, count: 2},
	} {
		matched, err := queryquality.NewLexicalMatcher(nil).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{entry}, EvidenceThreshold: test.threshold, EvidenceThresholdSet: true})
		if err != nil {
			t.Fatal(err)
		}
		candidate := matched.Candidates[0]
		if candidate.Qualified != test.qualified || candidate.PositiveEvidenceCount != test.count {
			t.Fatalf("threshold=%d candidate=%#v, want qualified=%v count=%d", test.threshold, candidate, test.qualified, test.count)
		}
	}
}

func TestUnresolvedSemanticEvidenceIsTriStateAndCannotQualifyAlone(t *testing.T) {
	plan := queryquality.QueryPlan{
		Excluded: []queryquality.Criterion{{Kind: "activity", Value: "skiing", Proof: "semantic"}},
		Required: []queryquality.Criterion{{Kind: "activity", Value: "snow", Proof: "semantic"}},
	}
	matched, err := queryquality.NewLexicalMatcher(fixedSemanticOutcomeEvaluator{outcome: "unknown"}).Match(context.Background(), queryquality.MatchRequest{
		Plan: plan, CorpusEntries: []cache.Entry{{Slug: "candidate", Title: "Candidate"}}, EvidenceThreshold: 0, EvidenceThresholdSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := matched.Candidates[0]
	if candidate.Eligible || candidate.Qualified || candidate.SemanticResolution != "unresolved" {
		t.Fatalf("candidate = %#v, want unresolved required semantic fail-closed and unqualified", candidate)
	}
	for _, group := range candidate.Groups {
		if group.SemanticOutcome != "unresolved" {
			t.Fatalf("semantic group = %#v, want unresolved", group)
		}
	}

	excludedOnly, err := queryquality.NewLexicalMatcher(fixedSemanticOutcomeEvaluator{outcome: "unknown"}).Match(context.Background(), queryquality.MatchRequest{
		Plan:          queryquality.QueryPlan{Excluded: []queryquality.Criterion{{Kind: "activity", Value: "skiing", Proof: "semantic"}}},
		CorpusEntries: []cache.Entry{{Slug: "excluded-unknown", Title: "Candidate"}}, EvidenceThreshold: 0, EvidenceThresholdSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !excludedOnly.Candidates[0].Eligible || excludedOnly.Candidates[0].Qualified || excludedOnly.Candidates[0].QualificationReason != "unresolved_semantic_evidence" {
		t.Fatalf("excluded-only unresolved candidate = %#v, want eligible but unqualified", excludedOnly.Candidates[0])
	}
}

type fixedSemanticOutcomeEvaluator struct{ outcome string }

func (e fixedSemanticOutcomeEvaluator) Evaluate(context.Context, queryquality.Criterion, cache.Entry) (queryquality.SemanticDecision, error) {
	return queryquality.SemanticDecision{Outcome: e.outcome}, nil
}

func TestNoGenericConstructorAliases(t *testing.T) {
	_, caller, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	corePath := filepath.Join(filepath.Dir(caller), "core.go")
	source, err := os.ReadFile(corePath)
	if err != nil {
		t.Fatalf("read core.go = %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), corePath, source, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse core.go = %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == "NewService" || fn.Name.Name == "NewServiceWithOptions" {
			t.Fatalf("found forbidden queryquality constructor alias %q", fn.Name.Name)
		}
	}
}

func TestQueryRetrievalPipelineBoundaryContracts(t *testing.T) {
	entries := []cache.Entry{
		{Slug: "coffee", Title: "Coffee", Body: "coffee"},
		{Slug: "tea", Title: "Tea", Body: "tea"},
	}
	expander := &trackingQueryExpander{}
	matcher := &recordingCandidateMatcher{}
	selector := &trackingResultSelector{}
	service := queryquality.NewQueryRetrievalPipeline(
		expander,
		matcher,
		selector,
		nil,
	)
	if _, err := service.Execute(context.Background(), &jsonlReader{data: []byte(`{"slug":"coffee","title":"Coffee","body":"coffee"}` + "\n" + `{"slug":"tea","title":"Tea","body":"tea"}` + "\n")}, query.Request{Query: "coffee", Mode: "wiki"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := expander.lastQuery; got != "coffee" {
		t.Fatalf("expander query = %q, want %q", got, "coffee")
	}
	if len(expander.lastPolicy.RequiredWhenExplicit) != len(queryquality.DefaultCriterionPolicy.RequiredWhenExplicit) {
		t.Fatalf("expander policy = %#v, want %v", expander.lastPolicy, queryquality.DefaultCriterionPolicy)
	}
	if len(matcher.lastCorpusEntries) != len(entries) {
		t.Fatalf("matcher corpus entries = %d, want %d", len(matcher.lastCorpusEntries), len(entries))
	}
	if matcher.lastPlan.RawQuery != "coffee" {
		t.Fatalf("matcher plan = %#v, want raw query coffee", matcher.lastPlan)
	}
	defaultOptions := queryquality.DefaultOptions()
	if selector.lastSelectionInput.Candidates == nil || selector.lastSelectionInput.Limit != defaultOptions.SelectionLimit || selector.lastSelectionInput.ExplorationSlots != defaultOptions.ExplorationSlots {
		t.Fatalf("selector input = %#v", selector.lastSelectionInput)
	}
	if matcher.lastCorpusEntries[0].Slug != "coffee" || matcher.lastCorpusEntries[1].Slug != "tea" {
		t.Fatalf("matcher corpus entries changed: %#v", matcher.lastCorpusEntries)
	}
}

func TestDecodeMinimalV1PlanRequiresStrictV1Contract(t *testing.T) {
	valid := `{"raw_query":"Taipei cafe","required":[{"kind":"location","value":"Taipei","terms":["Taipei"],"proof":"lexical"}],"excluded":[],"preferred":[],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`
	if _, err := queryquality.DecodeMinimalV1Plan(valid, "Taipei cafe"); err != nil {
		t.Fatalf("valid minimal-v1 plan rejected: %v", err)
	}
	for _, test := range []struct {
		name     string
		response string
		raw      string
	}{
		{name: "missing top level fields", response: `{}`, raw: "coffee"},
		{name: "unknown top level field", response: valid[:len(valid)-1] + `,"extra":1}`, raw: "Taipei cafe"},
		{name: "fallback true", response: strings.Replace(valid, `"fallback":false`, `"fallback":true`, 1), raw: "Taipei cafe"},
		{name: "supporting dimensions populated", response: strings.Replace(valid, `"supporting_dimensions":[]`, `"supporting_dimensions":[{"kind":"x","value":"y","terms":["y"],"proof":"lexical"}]`, 1), raw: "Taipei cafe"},
		{name: "empty plan", response: strings.Replace(valid, `{"kind":"location","value":"Taipei","terms":["Taipei"],"proof":"lexical"}`, ``, 1), raw: "Taipei cafe"},
		{name: "raw query mismatch", response: valid, raw: "different"},
		{name: "criterion missing proof", response: strings.Replace(valid, `,"proof":"lexical"`, ``, 1), raw: "Taipei cafe"},
		{name: "whitespace lexical term", response: strings.Replace(valid, `"terms":["Taipei"]`, `"terms":["   "]`, 1), raw: "Taipei cafe"},
		{name: "lexical criterion has no terms", response: strings.Replace(valid, `"terms":["Taipei"]`, `"terms":[]`, 1), raw: "Taipei cafe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := queryquality.DecodeMinimalV1Plan(test.response, test.raw); err == nil {
				t.Fatal("DecodePlan() error = nil, want contract rejection")
			}
		})
	}
}

func TestDecodePlanAllowsExtendedDimensionsAndAlternatives(t *testing.T) {
	response := `{"raw_query":"Taipei cafe","required":[{"kind":"location","value":"Taipei","terms":["Taipei"],"proof":"lexical"}],"excluded":[],"preferred":[],"goals":[],"supporting_dimensions":[{"kind":"mood","value":"quiet","terms":["quiet"],"proof":"lexical"}],"acceptable_alternatives":[{"kind":"topic","value":"tea","terms":["tea"],"proof":"lexical"}],"ambiguity":["or"],"fallback":false}`
	plan, err := queryquality.DecodePlan(response, "Taipei cafe")
	if err != nil {
		t.Fatalf("DecodePlan() error = %v, want generic acceptance", err)
	}
	if len(plan.SupportingDimensions) != 1 || len(plan.AcceptableAlternatives) != 1 {
		t.Fatalf("plan=%#v, want one supporting dimension and one alternative", plan)
	}
	if _, err := queryquality.DecodeMinimalV1Plan(response, "Taipei cafe"); err == nil {
		t.Fatal("DecodeMinimalV1Plan() error = nil, want extended plan rejection")
	}
}

func TestQueryPlanValidationRejectsWhitespaceTermsAndCannotMatchAllCandidates(t *testing.T) {
	plan := queryquality.QueryPlan{
		RawQuery: "coffee",
		Required: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"   "}, Proof: "lexical"}},
	}
	if err := queryquality.ValidateQueryPlan(plan); err == nil {
		t.Fatal("ValidateQueryPlan() error = nil, want lexical term rejection")
	}

	service := queryquality.NewQueryRetrievalPipeline(
		fixedQueryExpander{plan: plan},
		queryquality.NewLexicalMatcher(nil),
		queryquality.NewResultSelector(),
		nil,
	)
	if _, err := service.Execute(context.Background(), &jsonlReader{data: []byte(`{"slug":"x","title":"coffee shop","body":"coffee here"}\n`)}, query.Request{Query: "coffee"}); err == nil {
		t.Fatal("Execute() error = nil, want lexical term rejection in production path")
	}
}

func TestDefaultCriterionPolicyIsPlatformOwnedLifestyleV1(t *testing.T) {
	policy := queryquality.DefaultCriterionPolicy
	if !sameStrings(policy.RequiredWhenExplicit, []string{"location", "explicit_exclusion"}) {
		t.Fatalf("required policy = %v", policy.RequiredWhenExplicit)
	}
	if !sameStrings(policy.PreferredByDefault, []string{"venue_type", "activity", "audience", "setting"}) {
		t.Fatalf("preferred policy = %v", policy.PreferredByDefault)
	}
	if !sameStrings(policy.GoalsToExpand, []string{"suitability", "recommendation", "discovery"}) {
		t.Fatalf("goal policy = %v", policy.GoalsToExpand)
	}
}

func TestStructuredPlanPromptIsMinimalV1(t *testing.T) {
	provider := &promptProvider{response: `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}
	expander := queryquality.NewStructuredPlanExpander(provider, nil)
	if _, err := expander.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee", CriterionPolicy: queryquality.DefaultCriterionPolicy}); err != nil {
		t.Fatal(err)
	}
	wantSystem := `You produce a retrieval plan for a frozen Lifestyle concept corpus. Return exactly one JSON object and no markdown. The object fields and exact types are: raw_query string; required array of Criterion; excluded array of Criterion; preferred array of Criterion; goals array of Criterion; supporting_dimensions array of Criterion; acceptable_alternatives array of Criterion; ambiguity array of strings; fallback boolean. Every Criterion is exactly {kind:string,value:string,terms:array of strings,proof:"lexical" or "semantic"}. Every lexical Criterion needs at least one discovery term. Never output a string where an array or object is required. Be conservative: only explicit user constraints may be required or excluded; absent never means excluded. In this minimal variant, supporting_dimensions and acceptable_alternatives must be empty arrays and fallback must be false.`
	wantUser := `Raw query: "coffee"` + "\n" + `Criterion policy: {"required_when_explicit":["location","explicit_exclusion"],"preferred_by_default":["venue_type","activity","audience","setting"],"goals_to_expand":["suitability","recommendation","discovery"]}` + "\nInterpret the query into required, excluded, preferred and goals. Preserve the raw query exactly in raw_query. Return the single JSON object only."
	if provider.system != wantSystem || provider.user != wantUser {
		t.Fatalf("prompt mismatch:\nsystem=%q\nuser=%q", provider.system, provider.user)
	}
	if queryquality.StructuredPlanPromptID != "minimal-v1" {
		t.Fatalf("prompt ID = %q, want minimal-v1", queryquality.StructuredPlanPromptID)
	}
}

func TestSemanticRequiredAndExcludedFailClosedRoles(t *testing.T) {
	for _, test := range []struct {
		outcome  string
		required bool
		want     bool
	}{
		{outcome: "pass", required: true, want: true},
		{outcome: "fail", required: true, want: false},
		{outcome: "unknown", required: true, want: false},
		{outcome: "unavailable", required: true, want: false},
		{outcome: "pass", want: false},
		{outcome: "fail", want: true},
		{outcome: "unknown", want: true},
		{outcome: "unavailable", want: true},
	} {
		name := "excluded-" + test.outcome
		if test.required {
			name = "required-" + test.outcome
		}
		t.Run(name, func(t *testing.T) {
			plan := queryquality.QueryPlan{}
			criterion := queryquality.Criterion{Kind: "intent", Value: "coffee", Proof: "semantic"}
			if test.required {
				plan.Required = []queryquality.Criterion{criterion}
			} else {
				plan.Excluded = []queryquality.Criterion{criterion}
			}
			matched, err := queryquality.NewLexicalMatcher(fixedSemanticEvaluator{outcome: test.outcome}).Match(context.Background(), queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "candidate", Title: "Candidate"}}})
			if err != nil {
				t.Fatal(err)
			}
			if matched.Candidates[0].Eligible != test.want {
				t.Fatalf("eligible = %v, want %v", matched.Candidates[0].Eligible, test.want)
			}
		})
	}
}

func TestServicePreservesCanceledCorpusRead(t *testing.T) {
	service := queryquality.NewQueryRetrievalPipeline(
		queryQualityTestQueryExpander{},
		queryquality.NewLexicalMatcher(nil), queryquality.NewResultSelector(), nil,
	)
	_, err := service.Execute(context.Background(), &jsonlReader{readErr: context.Canceled}, query.Request{Query: "coffee"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
}

func TestMatchingAndSelectionPreserveCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := queryquality.QueryPlan{Preferred: []queryquality.Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}}}
	if _, err := queryquality.NewLexicalMatcher(nil).Match(ctx, queryquality.MatchRequest{Plan: plan, CorpusEntries: []cache.Entry{{Slug: "coffee"}}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Match() error = %v, want context.Canceled", err)
	}
	if _, err := queryquality.NewResultSelector().Select(ctx, queryquality.SelectionInput{Candidates: []queryquality.CandidateEvidence{{Slug: "coffee", Eligible: true}}, Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Select() error = %v, want context.Canceled", err)
	}
}

func TestProductionExecutorSynthesisCancellationDoesNotFallback(t *testing.T) {
	transport := &queryCancellationTransport{started: make(chan struct{}), canceled: make(chan struct{})}
	baseTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = baseTransport })

	synthesizer := query.NewService(cache.New(), nil, llm.NewClient("test"))
	legacy := &countingExecutor{}
	executor, err := queryquality.NewProductionExecutor(cache.New(), fakeProvider{response: `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}, legacy, synthesizer, queryquality.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, &jsonlReader{data: []byte(`{"slug":"coffee","title":"Coffee","body":"coffee"}` + "\n")}, query.Request{Query: "coffee", Mode: "wiki"})
		done <- err
	}()
	<-transport.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("executor did not return after cancellation")
	}
	if legacy.called {
		t.Fatal("cancellation must not invoke legacy fallback")
	}
}

func TestSelectorDeterministicReplayAndContextCancellation(t *testing.T) {
	input := queryquality.SelectionInput{
		Candidates: []queryquality.CandidateEvidence{
			{Slug: "a", Title: "A", Eligible: true, Qualified: true, Score: 3},
			{Slug: "b", Title: "B", Eligible: true, Qualified: true, Score: 2},
			{Slug: "c", Title: "C", Eligible: true, Qualified: true, Score: 1},
			{Slug: "d", Title: "D", Eligible: true, Qualified: true, Score: 0},
		},
		Limit:            2,
		ExplorationSlots: 2,
		Seed:             7,
	}
	first, err := queryquality.NewResultSelector().Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := queryquality.NewResultSelector().Select(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("selection replay differs: %s vs %s", firstJSON, secondJSON)
	}
	if len(first.Selected) == 0 {
		t.Fatal("selection result is empty")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := queryquality.NewResultSelector().Select(ctx, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("Select() error = %v, want context.Canceled", err)
	}
}

func TestProductionExpansionFailureAndInvalidJSONUseDeterministicFallback(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider queryquality.ChatProvider
	}{
		{name: "provider absent", provider: nil},
		{name: "provider failure", provider: fakeProvider{err: errors.New("provider unavailable")}},
		{name: "invalid JSON", provider: fakeProvider{response: "not-json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			legacy := fakeExecutor{}
			executor, err := queryquality.NewProductionExecutor(cache.New(), test.provider, legacy, nil, queryquality.DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			got, err := executor.Execute(context.Background(), &jsonlReader{data: []byte(`{"slug":"all-candidate","title":"all","body":""}` + "\n")}, query.Request{Query: "q", Mode: "wiki"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Results) != 0 || got.Status != "insufficient_evidence" || got.Reason != "no_qualified_evidence" {
				t.Fatalf("fallback result = %#v status=%q reason=%q, want truthful insufficient evidence", got.Results, got.Status, got.Reason)
			}
		})
	}
}

func TestProductionExpansionCancellationDoesNotFallback(t *testing.T) {
	started := make(chan struct{})
	provider := blockingProvider{started: started}
	legacy := &countingExecutor{}
	executor, err := queryquality.NewProductionExecutor(cache.New(), provider, legacy, nil, queryquality.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.Execute(ctx, &jsonlReader{data: []byte(`{"slug":"candidate","title":"candidate","body":""}` + "\n")}, query.Request{Query: "q"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute() did not return after cancellation")
	}
	if legacy.called {
		t.Fatal("cancellation must not invoke legacy fallback")
	}
}

func TestStructuredPlanExpanderPreservesCancellationBeforeFallbackWithoutProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expander := queryquality.NewStructuredPlanExpander(nil, nil)
	traced, ok := expander.(queryquality.TracedQueryExpander)
	if !ok {
		t.Fatal("structured plan expander should support tracing")
	}
	if _, _, err := traced.ExpandWithTrace(ctx, queryquality.ExpansionRequest{Query: "coffee", CriterionPolicy: queryquality.DefaultCriterionPolicy}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExpandPlanWithTrace() error = %v, want context.Canceled", err)
	}
	traced, ok = queryquality.NewStructuredPlanExpander(&fakeProvider{response: "{}"}, nil).(queryquality.TracedQueryExpander)
	if !ok {
		t.Fatal("structured plan expander should support tracing")
	}
	_, _, err := traced.ExpandWithTrace(context.Background(), queryquality.ExpansionRequest{Query: "coffee", CriterionPolicy: queryquality.DefaultCriterionPolicy})
	if err == nil {
		t.Fatal("ExpandPlanWithTrace() error = nil, want plan decode failure")
	}
}

func TestStructuredPlanExpanderCancellationFallbackPolicy(t *testing.T) {
	fallbackPlan := queryquality.QueryPlan{
		Preferred: []queryquality.Criterion{
			{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"},
		},
	}
	tests := []struct {
		name              string
		err               error
		wantFallbackCalls int
		wantErr           error
		requireNoPlan     bool
	}{
		{name: "provider-cancelled", err: context.Canceled, wantFallbackCalls: 0, wantErr: context.Canceled, requireNoPlan: true},
		{name: "provider-deadline", err: context.DeadlineExceeded, wantFallbackCalls: 0, wantErr: context.DeadlineExceeded, requireNoPlan: true},
		{name: "provider-failed-with-wrapped-deadline", err: context.DeadlineExceeded, wantFallbackCalls: 0, wantErr: context.DeadlineExceeded, requireNoPlan: true},
		{name: "provider-failed-triggers-fallback", err: errors.New("provider unavailable"), wantFallbackCalls: 1, wantErr: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fallback := &countingQueryExpander{plan: fallbackPlan}
			providerErr := test.err
			if test.name == "provider-failed-with-wrapped-deadline" {
				providerErr = fmt.Errorf("provider: %w", context.DeadlineExceeded)
			}
			expander := queryquality.NewStructuredPlanExpander(fakeProvider{err: providerErr}, fallback)
			traced, ok := expander.(queryquality.TracedQueryExpander)
			if !ok {
				t.Fatal("structured plan expander should support tracing")
			}

			plan, info, err := traced.ExpandWithTrace(context.Background(), queryquality.ExpansionRequest{Query: "coffee", CriterionPolicy: queryquality.DefaultCriterionPolicy})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ExpandPlanWithTrace() error = %v, want %v", err, test.wantErr)
			}
			if fallback.calls != test.wantFallbackCalls {
				t.Fatalf("fallback calls = %d, want %d", fallback.calls, test.wantFallbackCalls)
			}
			if test.wantErr == nil {
				if info.Source != "deterministic-fallback" {
					t.Fatalf("fallback source = %q, want deterministic-fallback", info.Source)
				}
				return
			}
			if test.requireNoPlan && (plan.RawQuery != "" || len(plan.Required) != 0 || len(plan.Excluded) != 0 || len(plan.Preferred) != 0 || len(plan.Goals) != 0 || len(plan.SupportingDimensions) != 0 || len(plan.AcceptableAlternatives) != 0 || len(plan.Ambiguity) != 0 || plan.Fallback != false) {
				t.Fatal("plan should be empty when provider cancellation propagates")
			}
		})
	}
}

func TestStructuredPlanExpanderZeroValueUsesDefaultDecoderAndReturnsProviderNotConfigured(t *testing.T) {
	var expander queryquality.StructuredPlanExpander
	if _, err := expander.Expand(context.Background(), queryquality.ExpansionRequest{Query: "coffee", CriterionPolicy: queryquality.DefaultCriterionPolicy}); err == nil {
		t.Fatal("ExpandPlan() error = nil, want provider_not_configured")
	} else {
		var expansionErr *queryquality.ExpansionError
		if !errors.As(err, &expansionErr) || expansionErr.Reason != "provider_not_configured" {
			t.Fatalf("ExpandPlan() err = %v, want ExpansionError reason provider_not_configured", err)
		}
	}
}

func TestProductionStructuredPlanPreservesLegacyExpandWireShape(t *testing.T) {
	provider := fakeProvider{response: `{"raw_query":"coffee","required":[{"kind":"location","value":"Taipei","terms":["Taipei","coffee"],"proof":"lexical"}],"excluded":[{"kind":"avoid","value":"crowded","terms":["crowded","coffee"],"proof":"lexical"}],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee","cafe"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`}
	legacy := &countingExecutor{}
	executor, err := queryquality.NewProductionExecutor(cache.New(), provider, legacy, nil, queryquality.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), &jsonlReader{data: []byte(`{"slug":"coffee","title":"Coffee","body":""}` + "\n")}, query.Request{Query: "coffee", Mode: "wiki"})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.called {
		t.Fatal("valid structured plan invoked legacy executor")
	}
	if result.Expand == nil || !sameStrings(result.Expand.Keywords, []string{"Taipei", "coffee", "crowded", "cafe"}) {
		t.Fatalf("Expand = %#v, want stable deduplicated plan terms", result.Expand)
	}
}

func containsSelected(values []queryquality.SelectedCandidate, slug string) bool {
	for _, value := range values {
		if value.Selected && value.Slug == slug {
			return true
		}
	}
	return false
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

type fakeProvider struct {
	response string
	err      error
}

func (f fakeProvider) Chat(context.Context, string, string) (string, error) { return f.response, f.err }

type promptProvider struct {
	response string
	system   string
	user     string
}

type fixedSemanticEvaluator struct{ outcome string }

func (e fixedSemanticEvaluator) Evaluate(context.Context, queryquality.Criterion, cache.Entry) (queryquality.SemanticDecision, error) {
	return queryquality.SemanticDecision{Outcome: e.outcome}, nil
}

type trackingQueryExpander struct {
	lastQuery  string
	lastPolicy queryquality.CriterionPolicy
}

func (e *trackingQueryExpander) Expand(ctx context.Context, request queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	e.lastQuery = request.Query
	e.lastPolicy = request.CriterionPolicy
	query := strings.Join(strings.Fields(request.Query), " ")
	if query == "" {
		return queryquality.QueryPlan{}, errors.New("empty query")
	}
	return queryquality.QueryPlan{
		RawQuery: request.Query,
		Preferred: []queryquality.Criterion{
			{Kind: "query", Value: query, Terms: []string{query}, Proof: "lexical"},
		},
	}, nil
}

type recordingCandidateMatcher struct {
	lastPlan          queryquality.QueryPlan
	lastCorpusEntries []cache.Entry
}

func (m *recordingCandidateMatcher) Match(_ context.Context, request queryquality.MatchRequest) (queryquality.EligibilityResult, error) {
	m.lastPlan = request.Plan
	m.lastCorpusEntries = request.CorpusEntries
	return queryquality.EligibilityResult{Candidates: []queryquality.CandidateEvidence{{Slug: "coffee", Title: "Coffee", Eligible: true}}}, nil
}

type trackingResultSelector struct {
	lastSelectionInput queryquality.SelectionInput
}

func (s *trackingResultSelector) Select(_ context.Context, input queryquality.SelectionInput) (queryquality.SelectionResult, error) {
	s.lastSelectionInput = input
	return queryquality.SelectionResult{Selected: []queryquality.SelectedCandidate{{Slug: "coffee", Selected: true, Score: 1}}}, nil
}

type queryQualityTestQueryExpander struct{}

func (queryQualityTestQueryExpander) Expand(context.Context, queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	return queryquality.QueryPlan{}, nil
}

type fixedQueryExpander struct {
	plan queryquality.QueryPlan
}

func (f fixedQueryExpander) Expand(context.Context, queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	return f.plan, nil
}

type countingQueryExpander struct {
	plan  queryquality.QueryPlan
	calls int
}

func (e *countingQueryExpander) Expand(context.Context, queryquality.ExpansionRequest) (queryquality.QueryPlan, error) {
	e.calls++
	return e.plan, nil
}

func (p *promptProvider) Chat(_ context.Context, system, user string) (string, error) {
	p.system, p.user = system, user
	return p.response, nil
}

type fakeExecutor struct{ result query.Result }

func (f fakeExecutor) Execute(context.Context, cache.Reader, query.Request) (query.Result, error) {
	return f.result, nil
}

type countingExecutor struct{ called bool }

func (e *countingExecutor) Execute(context.Context, cache.Reader, query.Request) (query.Result, error) {
	e.called = true
	return query.Result{}, nil
}

type blockingProvider struct{ started chan struct{} }

func (p blockingProvider) Chat(ctx context.Context, _, _ string) (string, error) {
	close(p.started)
	<-ctx.Done()
	return "", ctx.Err()
}

type queryCancellationTransport struct {
	started  chan struct{}
	canceled chan struct{}
}

func (t queryCancellationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	close(t.started)
	<-req.Context().Done()
	close(t.canceled)
	return nil, req.Context().Err()
}

type jsonlReader struct {
	data    []byte
	readErr error
}

func (r *jsonlReader) Prefix() string                                             { return "users/test/projects/test" }
func (r *jsonlReader) ReadFile(context.Context, string) ([]byte, error)           { return r.data, r.readErr }
func (r *jsonlReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) { return nil, nil }
func (r *jsonlReader) GetPage(context.Context, string, string) (*gcs.WikiPage, []byte, error) {
	return nil, nil, errors.New("unexpected page read")
}
