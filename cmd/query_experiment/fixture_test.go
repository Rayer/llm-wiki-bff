package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFixtureDecodersAreStrictAndKeepModelKeysPrivate(t *testing.T) {
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "models.json")
	writeTestFile(t, modelPath, `{"models":[{"id":"m1","provider":"fake","base_url":"http://127.0.0.1:1","model":"model-1","api_key":"secret-key"}]}`)
	models, err := readModelFixture(modelPath)
	if err != nil || len(models) != 1 || models[0].APIKey != "secret-key" {
		t.Fatalf("models=%#v err=%v", models, err)
	}
	encoded := marshalPublicJSON(models[0])
	if strings.Contains(encoded, "secret-key") || strings.Contains(encoded, "api_key") {
		t.Fatalf("public model serialization leaked credentials: %s", encoded)
	}
	for name, contents := range map[string]string{
		"unknown":   `{"models":[{"id":"m1","provider":"fake","base_url":"http://x","model":"m","api_key":"k","extra":true}]}`,
		"duplicate": `{"models":[{"id":"m1","provider":"fake","base_url":"http://x","model":"m","api_key":"k","model":"m2"}]}`,
	} {
		path := filepath.Join(dir, name+".json")
		writeTestFile(t, path, contents)
		if _, err := readModelFixture(path); err == nil {
			t.Fatalf("%s fixture accepted", name)
		}
	}
	if _, err := readPromptFixture(writeFixture(t, dir, "prompts.json", `{"prompts":[{"id":"p","system_template":"{{raw_query}}","user_template":"{{criterion_policy}}","extra":1}]}`)); err == nil {
		t.Fatal("prompt fixture accepted unknown field")
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureUsageAggregationSumsProviderAttemptsWithoutChangingFanoutLatency(t *testing.T) {
	total := fixtureUsage{}
	for _, usage := range []fixtureUsage{{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}, {PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11}, {PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}} {
		total = addFixtureUsage(total, usage)
	}
	if total != (fixtureUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}) {
		t.Fatalf("aggregated usage=%#v", total)
	}
	if elapsed := int64(120); elapsed != 120 {
		t.Fatalf("fanout latency was replaced by attempt sum: %d", elapsed)
	}
}

func TestFixtureSelectorsPreserveRequestedCartesianOrder(t *testing.T) {
	models := []modelFixtureEntry{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}
	profiles := []profileFixtureEntry{{ID: "p1"}}
	prompts := []promptFixtureEntry{{ID: "x"}, {ID: "y"}}
	got, err := selectFixtureMatrix(models, profiles, prompts, "m3,m1", "p1", "y,x")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"p1/y/m3", "p1/y/m1", "p1/x/m3", "p1/x/m1"}
	ids := make([]string, 0, len(got))
	for _, variant := range got {
		ids = append(ids, variant.Profile.ID+"/"+variant.Prompt.ID+"/"+variant.Model.ID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("variant order=%v want=%v", ids, want)
	}
	if got[0].VariantID == got[1].VariantID || got[0].VariantID == got[2].VariantID {
		t.Fatalf("variant IDs are not unique: %#v", got)
	}
}

func TestFixtureVariantIdentityIncludesProfileDigest(t *testing.T) {
	models := []modelFixtureEntry{{ID: "m"}}
	prompts := []promptFixtureEntry{{ID: "p"}}
	left := []profileFixtureEntry{{ID: "same", RequiredWhenExplicit: []string{"topic"}}}
	right := []profileFixtureEntry{{ID: "same", RequiredWhenExplicit: []string{"location"}}}
	leftVariants, err := selectFixtureMatrix(models, left, prompts, "", "same", "")
	if err != nil {
		t.Fatal(err)
	}
	rightVariants, err := selectFixtureMatrix(models, right, prompts, "", "same", "")
	if err != nil {
		t.Fatal(err)
	}
	if leftVariants[0].VariantID == rightVariants[0].VariantID {
		t.Fatalf("variant IDs=%q,%q", leftVariants[0].VariantID, rightVariants[0].VariantID)
	}
	options := experimentOptions{
		selectionLimit:        10,
		explorationSlots:      1,
		explorationSlotsSet:   true,
		keywordsPerAttempt:    24,
		expansionAttempts:     3,
		rareDocumentFrequency: 1,
		evidenceThreshold:     2,
		evidenceThresholdSet:  true,
	}
	leftArtifact, err := fixtureVariantID(leftVariants[0].VariantID, options, preparedSnapshot{digest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	rightArtifact, err := fixtureVariantID(rightVariants[0].VariantID, options, preparedSnapshot{digest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if leftArtifact == rightArtifact {
		t.Fatalf("artifact identities not split by profile content: %q", leftArtifact)
	}
	repeatArtifact, err := fixtureVariantID(leftVariants[0].VariantID, options, preparedSnapshot{digest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if leftArtifact != repeatArtifact {
		t.Fatalf("artifact identity is not deterministic: %q != %q", leftArtifact, repeatArtifact)
	}
}

func TestFixtureVariantIDDiffersByPromptModelAndCorpusDigestInputs(t *testing.T) {
	models := []modelFixtureEntry{{ID: "m1"}, {ID: "m2"}}
	prompts := []promptFixtureEntry{{ID: "p1"}, {ID: "p2"}}
	profiles := []profileFixtureEntry{{ID: "same", RequiredWhenExplicit: []string{"topic"}, PreferredByDefault: []string{"system"}, GoalsToExpand: []string{"discovery"}}}
	profileVariants, err := selectFixtureMatrix(models, profiles, prompts, "", "same", "")
	if err != nil {
		t.Fatal(err)
	}
	options := experimentOptions{
		selectionLimit:        10,
		explorationSlots:      1,
		explorationSlotsSet:   true,
		keywordsPerAttempt:    24,
		expansionAttempts:     3,
		rareDocumentFrequency: 7,
		seed:                  int64Ptr(123),
		evidenceThreshold:     3,
		evidenceThresholdSet:  true,
	}
	ids := map[string]string{
		"profile-m1-p1-a": "",
		"profile-m2-p1-a": "",
		"profile-m1-p2-a": "",
	}
	first := profileVariants[0]
	second := profileVariants[1]
	third := profileVariants[2]
	for name, variant := range map[string]fixtureVariant{"profile-m1-p1-a": first, "profile-m2-p1-a": second, "profile-m1-p2-a": third} {
		variantID, err := fixtureVariantID(variant.VariantID, options, preparedSnapshot{digest: strings.Repeat("a", 64)})
		if err != nil {
			t.Fatal(err)
		}
		if ids[name] == "" {
			ids[name] = variantID
		}
	}
	ids["profile-m1-p1-b"], err = fixtureVariantID(first.VariantID, options, preparedSnapshot{digest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if ids["profile-m1-p1-a"] == ids["profile-m2-p1-a"] || ids["profile-m1-p1-a"] == ids["profile-m1-p2-a"] || ids["profile-m1-p1-a"] == ids["profile-m1-p1-b"] {
		t.Fatalf("fixture identities not sensitive to all variant factors: %v", ids)
	}
}

func TestFixtureVariantIDUsesBoundedDigestTokensAndSegmentSafePathIDs(t *testing.T) {
	models := []modelFixtureEntry{{ID: "m"}}
	prompts := []promptFixtureEntry{{ID: "p"}}
	left := []profileFixtureEntry{{ID: "same", RequiredWhenExplicit: []string{"topic"}, PreferredByDefault: []string{"system"}, GoalsToExpand: []string{"discovery"}}}
	right := []profileFixtureEntry{{ID: "same", RequiredWhenExplicit: []string{"location"}, PreferredByDefault: []string{"system"}, GoalsToExpand: []string{"discovery"}}}
	leftVariants, err := selectFixtureMatrix(models, left, prompts, "", "same", "")
	if err != nil {
		t.Fatal(err)
	}
	rightVariants, err := selectFixtureMatrix(models, right, prompts, "", "same", "")
	if err != nil {
		t.Fatal(err)
	}
	options := experimentOptions{
		selectionLimit:        10,
		explorationSlots:      1,
		explorationSlotsSet:   true,
		keywordsPerAttempt:    24,
		expansionAttempts:     3,
		rareDocumentFrequency: 7,
		seed:                  int64Ptr(123),
		evidenceThreshold:     3,
		evidenceThresholdSet:  true,
	}
	leftCorpus, err := fixtureVariantID(leftVariants[0].VariantID, options, preparedSnapshot{digest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	rightCorpus, err := fixtureVariantID(rightVariants[0].VariantID, options, preparedSnapshot{digest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	leftCorpusDifferent, err := fixtureVariantID(leftVariants[0].VariantID, options, preparedSnapshot{digest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if leftCorpus == rightCorpus || leftCorpus == leftCorpusDifferent {
		t.Fatalf("variant IDs not separated by profile/corpus digest: %q / %q / %q", leftCorpus, rightCorpus, leftCorpusDifferent)
	}
	assertBoundedPathSegments(t, filepath.Join("artifacts", leftCorpus, receiptSegment("case/with/slash"), "run-1"))
	assertBoundedPathSegments(t, filepath.Join("artifacts", rightCorpus, receiptSegment("case/with/slash"), "run-2"))
	assertBoundedPathSegments(t, filepath.Join("artifacts", leftCorpusDifferent, receiptSegment("case/with/slash"), "run-3"))
	if !strings.HasPrefix(leftCorpus, fixtureVariantIDPrefix) || len(leftCorpus) > maxPortablePathSegmentBytes {
		t.Fatalf("variant identity not bounded/prefixed: %q", leftCorpus)
	}
}

func TestFixtureVariantIDStaysBoundedWithLongValidInputsAndMaxOptions(t *testing.T) {
	huge := strings.Repeat("x", 112)
	models := []modelFixtureEntry{{ID: huge + "-model"}}
	prompts := []promptFixtureEntry{{ID: huge + "-prompt"}}
	profiles := []profileFixtureEntry{{ID: huge + "-profile", RequiredWhenExplicit: []string{"topic"}, PreferredByDefault: []string{"system"}, GoalsToExpand: []string{"discovery"}}}
	variants, err := selectFixtureMatrix(models, profiles, prompts, "", huge+"-profile", "")
	if err != nil {
		t.Fatal(err)
	}
	options := experimentOptions{
		selectionLimit:        1000,
		explorationSlots:      1000,
		explorationSlotsSet:   true,
		keywordsPerAttempt:    100,
		expansionAttempts:     10,
		rareDocumentFrequency: 1000,
		seed:                  int64Ptr(999),
		evidenceThreshold:     10,
		evidenceThresholdSet:  true,
	}
	id, err := fixtureVariantID(variants[0].VariantID, options, preparedSnapshot{digest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	assertBoundedPathSegments(t, filepath.Join("artifacts", id, "case", "run-1"))
}

func TestDigestTokenRejectsMalformedSHA256(t *testing.T) {
	if _, err := digestToken("sha256:not-hex"); err == nil {
		t.Fatal("malformed digest accepted")
	}
	if _, err := digestToken(strings.Repeat("a", 63)); err == nil {
		t.Fatal("short digest accepted")
	}
	if got, err := digestToken(strings.Repeat("a", 64)); err != nil || got != strings.Repeat("a", artifactDigestTokenLength) {
		t.Fatalf("digest token=%q err=%v", got, err)
	}
}

func TestFixtureProfileUsesProductionProfileDigest(t *testing.T) {
	fixture := profileFixtureEntry{ID: "corpus-derived-tech-document-v1", RequiredWhenExplicit: []string{"topic"}, PreferredByDefault: []string{"system"}, GoalsToExpand: []string{"discovery"}}
	path := filepath.Join(t.TempDir(), "profiles.json")
	writeTestFile(t, path, `{"profiles":[{"id":"corpus-derived-tech-document-v1","required_when_explicit":["topic"],"preferred_by_default":["system"],"goals_to_expand":["discovery"]}]}`)
	loaded, err := readProfileFixture(path)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("loaded profiles=%#v err=%v", loaded, err)
	}
	production, err := loaded[0].retrievalProfile()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := production.Digest()
	if err != nil {
		t.Fatal(err)
	}
	other, err := production.ValidatedCopy()
	if err != nil {
		t.Fatal(err)
	}
	otherDigest, err := other.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != otherDigest || production.ID != fixture.ID || production.CriterionPolicy.RequiredWhenExplicit[0] != "topic" {
		t.Fatalf("fixture/production profile mismatch: fixture=%+v production=%+v digests=%q/%q", fixture, production, digest, otherDigest)
	}
}

func TestFixturePromptRenderingUsesOnlyNarrowPlaceholders(t *testing.T) {
	prompt := promptFixtureEntry{ID: "p", SystemTemplate: "sys {{raw_query}}", UserTemplate: "user {{criterion_policy}} / {{raw_query}}"}
	policy := CriterionPolicy{RequiredWhenExplicit: []string{"location"}, PreferredByDefault: []string{"topic"}, GoalsToExpand: []string{"discovery"}}
	rendered, err := renderFixturePrompt(prompt, "coffee shops", policy)
	if err != nil || rendered.System != "sys coffee shops" || !strings.Contains(rendered.User, `"required_when_explicit":["location"]`) || !strings.Contains(rendered.User, "coffee shops") {
		t.Fatalf("rendered=%#v err=%v", rendered, err)
	}
	prompt.UserTemplate = "{{unknown}}"
	if _, err := renderFixturePrompt(prompt, "coffee", policy); err == nil {
		t.Fatal("unknown prompt placeholder accepted")
	}
}

func TestFixtureModelCallSendsSelectedEndpointModelAndKeyOnlyOnHTTP(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotAuth = request.Header.Get("Authorization")
		_ = json.NewDecoder(request.Body).Decode(&gotBody)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"{\"preferred\":[{\"kind\":\"topic\",\"value\":\"coffee\",\"terms\":[\"coffee\"]}]}"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}))
	defer server.Close()
	temperature := 0.2
	model := modelFixtureEntry{ID: "m", Provider: "fake", BaseURL: server.URL, Model: "selected-model", APIKey: "secret-key", Temperature: &temperature, Reasoning: "low"}
	call, err := callFixtureModel(context.Background(), model, "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/chat/completions" || gotAuth != "Bearer secret-key" {
		t.Fatalf("request path/auth=%q/%q", gotPath, gotAuth)
	}
	gotModel, _ = gotBody["model"].(string)
	if gotModel != "selected-model" || gotBody["temperature"] != 0.2 || gotBody["reasoning_effort"] != "low" {
		t.Fatalf("request body=%#v", gotBody)
	}
	if call.Content == "" || call.Usage.TotalTokens != 10 {
		t.Fatalf("call=%#v", call)
	}
}

func TestFixtureRunWritesEightReceiptsAndSummaryWithoutKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(validFixtureResponse())
	}))
	defer server.Close()
	root := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	writeTestFile(t, filepath.Join(root, "cache", "suggested_queries.json"), `{"version":2,"queries":[],"candidates":[],"updated_at":"2026-08-20T00:00:00Z"}`)
	dir := t.TempDir()
	modelPath := writeFixture(t, dir, "models.json", `{"models":[{"id":"m","provider":"fake","base_url":"`+server.URL+`","model":"selected","api_key":"fixture-secret"}]}`)
	profilePath := writeFixture(t, dir, "profiles.json", `{"profiles":[{"id":"profile","required_when_explicit":[],"preferred_by_default":["topic"],"goals_to_expand":[]}]}`)
	promptPath := writeFixture(t, dir, "prompts.json", `{"prompts":[{"id":"prompt","system_template":"system {{raw_query}}","user_template":"user {{criterion_policy}} {{raw_query}}"}]}`)
	casesPath := writeFixture(t, dir, "cases.jsonl", `{"id":"case","query":"coffee","mode":"wiki"}`+"\n")
	artifacts := filepath.Join(dir, "artifacts")
	summaryPath := filepath.Join(dir, "summary.json")
	var output strings.Builder
	err := runExperiment(context.Background(), experimentOptions{
		snapshotPath: root, casesPath: casesPath, runs: 1, service: serviceQueryRetrievalLegacy,
		selectionLimit: 1, explorationSlots: 0, explorationSlotsSet: true, expansionAttempts: 3, seed: int64Ptr(7),
		modelFixturePath: modelPath, profileFixturePath: profilePath, promptFixturePath: promptPath,
		artifactsDir: artifacts, summaryPath: summaryPath,
	}, dependencies{now: time.Now, stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "fixture-secret") {
		t.Fatalf("JSONL leaked key: %s", output.String())
	}
	var record resultRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatal(err)
	}
	if record.VariantID == "" || record.ProfileID != "profile" || record.PromptID != "prompt" || record.Provider != "fake" || record.Model != "selected" {
		t.Fatalf("record identity=%#v", record)
	}
	if record.EvidenceThreshold != 2 || !strings.HasPrefix(record.VariantID, fixtureVariantIDPrefix) {
		t.Fatalf("record evidence=%d variant=%q, want hashed bounded variant identity with prefix", record.EvidenceThreshold, record.VariantID)
	}
	variantDir := filepath.Join(artifacts, record.VariantID, "case", "run-1")
	expansionData, err := os.ReadFile(filepath.Join(variantDir, "expansion.output.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expansion fixtureExpansionOutput
	if err := json.Unmarshal(expansionData, &expansion); err != nil || expansion.Usage != (fixtureUsage{PromptTokens: 6, CompletionTokens: 9, TotalTokens: 15}) {
		t.Fatalf("expansion aggregation=%#v err=%v", expansion, err)
	}
	for _, name := range []string{"request.json", "expansion.input.json", "expansion.output.json", "matching.input.json", "matching.output.json", "selection.input.json", "selection.output.json", "final.json"} {
		data, err := os.ReadFile(filepath.Join(variantDir, name))
		if err != nil {
			t.Fatalf("receipt %s: %v", name, err)
		}
		if strings.Contains(string(data), "fixture-secret") {
			t.Fatalf("receipt %s leaked key", name)
		}
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil || object["attempt_id"] == nil || object["variant_id"] == nil {
			t.Fatalf("receipt %s metadata=%v err=%v", name, object, err)
		}
		if object["evidence_threshold"] != float64(2) {
			t.Fatalf("receipt %s threshold=%v, want 2", name, object["evidence_threshold"])
		}
		for _, key := range []string{"snapshot_identity", "corpus_sha256", "suggested_queries_sha256"} {
			if value, ok := object[key].(string); !ok || value == "" {
				t.Fatalf("receipt %s provenance %s=%v, want non-empty string", name, key, object[key])
			}
		}
	}
	requestData, err := os.ReadFile(filepath.Join(variantDir, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	matchingData, err := os.ReadFile(filepath.Join(variantDir, "matching.input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var matching fixtureMatchingInput
	var request fixtureRequestReceipt
	if err := json.Unmarshal(requestData, &request); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(matchingData, &matching); err != nil {
		t.Fatal(err)
	}
	if got, want := matching.EvidenceThreshold, record.EvidenceThreshold; got != want {
		t.Fatalf("matching.evidence_threshold=%d, want %d", got, want)
	}
	if got, want := matching.RareKeywordMaxDocumentFrequency, request.RareKeywordMaxDocumentFrequency; got != want {
		t.Fatalf("matching.rare_keyword_max_document_frequency=%d, want %d", got, want)
	}
	if matching.FallbackQualificationAllowed != false {
		t.Fatalf("matching.fallback_qualification_allowed=%v, want false", matching.FallbackQualificationAllowed)
	}
	var summary map[string]any
	data, err := os.ReadFile(summaryPath)
	if err != nil || json.Unmarshal(data, &summary) != nil || summary["variants"] == nil || summary["attempt_count"] != float64(1) {
		t.Fatalf("summary=%s err=%v", data, err)
	}
	if strings.Contains(string(data), "fixture-secret") || strings.Contains(string(data), "api_key") || summary["evidence_threshold"] != float64(2) {
		t.Fatalf("summary leaked credentials: %s", data)
	}
}

func TestFixtureAttemptWritesTimingFieldsInRecordAndFinalReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"{\"preferred\":[{\"kind\":\"topic\",\"value\":\"coffee\",\"terms\":[\"coffee\"]}]}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer server.Close()
	root := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"coffee"}`+"\n")
	writeTestFile(t, filepath.Join(root, "cache", "suggested_queries.json"), `{"version":2,"queries":[],"candidates":[],"updated_at":"2026-08-20T00:00:00Z"}`)
	dir := t.TempDir()
	modelPath := writeFixture(t, dir, "models.json", `{"models":[{"id":"m","provider":"fake","base_url":"`+server.URL+`","model":"selected","api_key":"fixture-secret"}]}`)
	profilePath := writeFixture(t, dir, "profiles.json", `{"profiles":[{"id":"profile","required_when_explicit":[],"preferred_by_default":["topic"],"goals_to_expand":[]}]}`)
	promptPath := writeFixture(t, dir, "prompts.json", `{"prompts":[{"id":"prompt","system_template":"system {{raw_query}}","user_template":"user {{criterion_policy}} {{raw_query}}"}]}`)
	casesPath := writeFixture(t, dir, "cases.jsonl", `{"id":"case","query":"coffee","mode":"wiki"}`+"\n")
	artifacts := filepath.Join(dir, "artifacts")
	var output strings.Builder
	received := time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC)
	runCompleted := received.Add(600 * time.Millisecond)
	next := scriptedNow(
		received,
		received.Add(20*time.Millisecond),
		received.Add(120*time.Millisecond),
		received.Add(130*time.Millisecond),
		received.Add(180*time.Millisecond),
		runCompleted,
	)
	err := runExperiment(context.Background(), experimentOptions{
		snapshotPath: root, casesPath: casesPath, runs: 1, service: serviceQueryRetrievalLegacy,
		selectionLimit: 1, explorationSlots: 0, explorationSlotsSet: true, seed: int64Ptr(7),
		modelFixturePath: modelPath, profileFixturePath: profilePath, promptFixturePath: promptPath,
		artifactsDir: artifacts,
	}, dependencies{now: next, stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	var record resultRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatal(err)
	}
	if record.QueryReceivedAt != received.Format(time.RFC3339Nano) || record.RunCompletedAt != runCompleted.Format(time.RFC3339Nano) || record.DurationMS != 600 {
		t.Fatalf("record timings=%#v", record)
	}
	variantDir := filepath.Join(artifacts, record.VariantID, "case", "run-1")
	finalReceiptContents, err := os.ReadFile(filepath.Join(variantDir, "final.json"))
	if err != nil {
		t.Fatal(err)
	}
	var final fixtureFinalReceipt
	if err := json.Unmarshal(finalReceiptContents, &final); err != nil {
		t.Fatal(err)
	}
	if final.QueryReceivedAt != received.Format(time.RFC3339Nano) || final.RunCompletedAt != runCompleted.Format(time.RFC3339Nano) || final.DurationMS != 600 {
		t.Fatalf("final receipt timings=%#v", final)
	}
}

func TestFixtureZeroQualifiedAttemptWritesStatusReasonInResultsAndFinalReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(validFixtureResponse())
	}))
	defer server.Close()
	root := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee-guide","title":"Coffee Guide","body":"private term: coffee"}`+"\n"+`{"slug":"other","title":"Other coffee","body":"coffee"}`+"\n")
	writeTestFile(t, filepath.Join(root, "cache", "suggested_queries.json"), `{"version":2,"queries":[],"candidates":[],"updated_at":"2026-08-20T00:00:00Z"}`)
	dir := t.TempDir()
	modelPath := writeFixture(t, dir, "models.json", `{"models":[{"id":"m","provider":"fake","base_url":"`+server.URL+`","model":"selected","api_key":"fixture-secret"}]}`)
	profilePath := writeFixture(t, dir, "profiles.json", `{"profiles":[{"id":"profile","required_when_explicit":[],"preferred_by_default":["topic"],"goals_to_expand":[]}]}`)
	promptPath := writeFixture(t, dir, "prompts.json", `{"prompts":[{"id":"prompt","system_template":"system {{raw_query}}","user_template":"user {{criterion_policy}} {{raw_query}}"}]}`)
	casesPath := writeFixture(t, dir, "cases.jsonl", `{"id":"case","query":"coffee","mode":"wiki"}`+"\n")
	artifacts := filepath.Join(dir, "artifacts")
	var output strings.Builder
	if err := runExperiment(context.Background(), experimentOptions{
		snapshotPath: root, casesPath: casesPath, runs: 1, service: serviceQueryRetrievalLegacy,
		selectionLimit: 1, explorationSlots: 0, explorationSlotsSet: true, expansionAttempts: 1, seed: int64Ptr(7),
		modelFixturePath: modelPath, profileFixturePath: profilePath, promptFixturePath: promptPath,
		artifactsDir: artifacts, evidenceThreshold: 99, evidenceThresholdSet: true,
	}, dependencies{now: time.Now, stdout: &output}); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatal(err)
	}
	if got, want := record["outcome"], "retrieval_miss"; got != want {
		t.Fatalf("results outcome=%v want=%q", got, want)
	}
	if got, want := record["status"], "insufficient_evidence"; got != want {
		t.Fatalf("results status=%v want=%q", got, want)
	}
	if got, want := record["reason"], "no_qualified_evidence"; got != want {
		t.Fatalf("results reason=%v want=%q", got, want)
	}
	variantDir := filepath.Join(artifacts, record["variant_id"].(string), "case", "run-1")
	finalReceiptContents, err := os.ReadFile(filepath.Join(variantDir, "final.json"))
	if err != nil {
		t.Fatal(err)
	}
	var final map[string]any
	if err := json.Unmarshal(finalReceiptContents, &final); err != nil {
		t.Fatal(err)
	}
	if got, want := final["status"], "insufficient_evidence"; got != want {
		t.Fatalf("final status=%v want=%q", got, want)
	}
	if got, want := final["reason"], "no_qualified_evidence"; got != want {
		t.Fatalf("final reason=%v want=%q", got, want)
	}
}

func TestFixtureNonemptyAttemptWritesOkAndQualifiedEvidenceInReceipts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(validFixtureResponse())
	}))
	defer server.Close()
	root := filepath.Join(t.TempDir(), "snapshot")
	writeTestFile(t, filepath.Join(root, "cache", "concepts.jsonl"), `{"slug":"coffee","title":"Coffee","body":"private term: coffee"}`+"\n")
	writeTestFile(t, filepath.Join(root, "cache", "suggested_queries.json"), `{"version":2,"queries":[],"candidates":[],"updated_at":"2026-08-20T00:00:00Z"}`)
	dir := t.TempDir()
	modelPath := writeFixture(t, dir, "models.json", `{"models":[{"id":"m","provider":"fake","base_url":"`+server.URL+`","model":"selected","api_key":"fixture-secret"}]}`)
	profilePath := writeFixture(t, dir, "profiles.json", `{"profiles":[{"id":"profile","required_when_explicit":[],"preferred_by_default":["topic"],"goals_to_expand":[]}]}`)
	promptPath := writeFixture(t, dir, "prompts.json", `{"prompts":[{"id":"prompt","system_template":"system {{raw_query}}","user_template":"user {{criterion_policy}} {{raw_query}}"}]}`)
	casesPath := writeFixture(t, dir, "cases.jsonl", `{"id":"case","query":"coffee","mode":"wiki"}`+"\n")
	artifacts := filepath.Join(dir, "artifacts")
	var output strings.Builder
	if err := runExperiment(context.Background(), experimentOptions{
		snapshotPath: root, casesPath: casesPath, runs: 1, service: serviceQueryRetrievalLegacy,
		selectionLimit: 1, explorationSlots: 0, explorationSlotsSet: true, seed: int64Ptr(7),
		modelFixturePath: modelPath, profileFixturePath: profilePath, promptFixturePath: promptPath,
		artifactsDir: artifacts,
	}, dependencies{now: time.Now, stdout: &output}); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatal(err)
	}
	if got, want := record["status"], "ok"; got != want {
		t.Fatalf("results status=%q want=%q", got, want)
	}
	if got, want := record["reason"], "qualified_evidence"; got != want {
		t.Fatalf("results reason=%q want=%q", got, want)
	}
	if got, want := record["outcome"], "success"; got != want {
		t.Fatalf("outcome=%v want=%q", got, want)
	}
	variantDir := filepath.Join(artifacts, record["variant_id"].(string), "case", "run-1")
	finalReceiptContents, err := os.ReadFile(filepath.Join(variantDir, "final.json"))
	if err != nil {
		t.Fatal(err)
	}
	var final map[string]any
	if err := json.Unmarshal(finalReceiptContents, &final); err != nil {
		t.Fatal(err)
	}
	if got, want := final["status"], "ok"; got != want {
		t.Fatalf("final status=%q want=%q", got, want)
	}
	if got, want := final["reason"], "qualified_evidence"; got != want {
		t.Fatalf("final reason=%q want=%q", got, want)
	}
}

func validFixtureResponse() []byte {
	content := `{"raw_query":"coffee","required":[],"excluded":[],"preferred":[{"kind":"topic","value":"coffee","terms":["coffee"],"proof":"lexical"}],"goals":[],"supporting_dimensions":[],"acceptable_alternatives":[],"ambiguity":[],"fallback":false}`
	response, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		"usage":   map[string]int{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
	})
	return response
}

func TestSummaryMetricsUseExplicitRepeatedRunDenominators(t *testing.T) {
	makeAttempt := func(run int, slugs []string, scores []int, fallback bool) fixtureAttempt {
		results := make([]resultIdentity, 0, len(slugs))
		candidates := make([]CandidateEvidence, 0, len(slugs))
		decisions := make([]SelectedCandidate, 0, len(slugs))
		for i, slug := range slugs {
			results = append(results, resultIdentity{Slug: slug, Type: "concept"})
			candidates = append(candidates, CandidateEvidence{Slug: slug, Eligible: true, Score: scores[i]})
			decisions = append(decisions, SelectedCandidate{Slug: slug, Selected: true, Score: 1})
		}
		return fixtureAttempt{Record: resultRecord{VariantID: "v", CaseID: "c", Results: results}, Case: caseInput{ID: "c", KnownPositiveSlugs: []string{"a"}, ForbiddenResultSlugs: []string{"z"}}, Candidates: candidates, Decisions: decisions, EffectiveSeed: 3, SelectionInputDigest: "same", Fallback: fallback, LatencyMS: int64(run), Usage: fixtureUsage{TotalTokens: 5}}
	}
	metrics := aggregateSummaryMetrics([]fixtureAttempt{
		makeAttempt(1, []string{"a", "b", "c"}, []int{1, 2, 3}, true),
		makeAttempt(2, []string{"a", "b", "d", "e", "f"}, []int{1, 4, 3, 2, 1}, false),
	})
	if metrics.AttemptCount != 2 || metrics.Under5Count != 1 || metrics.Under5Rate != 0.5 || metrics.RecoverableUnder5CaseCount != 1 || metrics.AlwaysUnder5CaseCount != 0 {
		t.Fatalf("count metrics=%#v", metrics)
	}
	if metrics.ExactResultSetMatchDenominator != 1 || metrics.ExactResultSetMatchCount != 0 || metrics.PairwiseComparisonCount != 1 || metrics.MeanPairwiseTop5Jaccard != 1.0/3.0 {
		t.Fatalf("repetition metrics=%#v", metrics)
	}
	if metrics.ScoreChangedCandidateDenominator != 2 || metrics.ScoreChangedCandidateCount != 1 || metrics.ExactSelectionReplayDenominator != 1 || metrics.ExactSelectionReplayCount != 0 {
		t.Fatalf("drift/replay metrics=%#v", metrics)
	}
	if metrics.KnownPositiveRecallAt5 != 1 || metrics.ForbiddenResultViolationDenominator != 2 || metrics.ForbiddenResultViolationCount != 0 {
		t.Fatalf("label metrics=%#v", metrics)
	}
}

func TestSummaryMetricsCausalAndOperationalCoverage(t *testing.T) {
	makeAttempt := func(caseID string, results int, latency int64, usage fixtureUsage, positives, forbidden []string) fixtureAttempt {
		identities := make([]resultIdentity, results)
		for i := range identities {
			identities[i] = resultIdentity{Slug: fmt.Sprintf("result-%d", i)}
		}
		return fixtureAttempt{
			Record:    resultRecord{VariantID: "v", CaseID: caseID, Results: identities},
			Case:      caseInput{ID: caseID, KnownPositiveSlugs: positives, ForbiddenResultSlugs: forbidden},
			LatencyMS: latency,
			Usage:     usage,
		}
	}

	tests := []struct {
		name     string
		attempts []fixtureAttempt
		check    func(*testing.T, summaryMetrics)
	}{
		{
			name:     "zero result attempt count and rate",
			attempts: []fixtureAttempt{makeAttempt("zero", 0, 1, fixtureUsage{}, nil, nil)},
			check: func(t *testing.T, got summaryMetrics) {
				if got.ZeroResultCount != 1 || got.ZeroResultRate != 1 || got.Under5Count != 1 || got.Under5Rate != 1 {
					t.Fatalf("metrics=%#v", got)
				}
			},
		},
		{
			name:     "always under five",
			attempts: []fixtureAttempt{makeAttempt("always", 1, 1, fixtureUsage{}, nil, nil), makeAttempt("always", 4, 2, fixtureUsage{}, nil, nil)},
			check: func(t *testing.T, got summaryMetrics) {
				if got.AlwaysUnder5CaseCount != 1 || got.AlwaysUnder5CaseRate != 1 || got.RecoverableUnder5CaseCount != 0 {
					t.Fatalf("metrics=%#v", got)
				}
			},
		},
		{
			name:     "recoverable is distinct from always",
			attempts: []fixtureAttempt{makeAttempt("recoverable", 2, 1, fixtureUsage{}, nil, nil), makeAttempt("recoverable", 5, 2, fixtureUsage{}, nil, nil)},
			check: func(t *testing.T, got summaryMetrics) {
				if got.RecoverableUnder5CaseCount != 1 || got.RecoverableUnder5CaseRate != 1 || got.AlwaysUnder5CaseCount != 0 {
					t.Fatalf("metrics=%#v", got)
				}
			},
		},
		{
			name:     "zero denominators are zero rates",
			attempts: []fixtureAttempt{makeAttempt("labels", 1, 1, fixtureUsage{}, []string{""}, []string{""})},
			check: func(t *testing.T, got summaryMetrics) {
				if got.ExactResultSetMatchRate != 0 || got.ScoreChangedCandidateRate != 0 || got.ExactSelectionReplayRate != 0 || got.KnownPositiveRecallAt5 != 0 || got.KnownPositiveRecallAt10 != 0 || got.ForbiddenResultViolationRate != 0 || got.KnownPositiveRecallAt5Denominator != 0 || got.ForbiddenResultViolationDenominator != 0 {
					t.Fatalf("metrics=%#v", got)
				}
			},
		},
		{
			name: "ordered latency percentile and token usage",
			attempts: []fixtureAttempt{
				makeAttempt("ops", 1, 90, fixtureUsage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}, nil, nil),
				makeAttempt("ops", 1, 10, fixtureUsage{}, nil, nil),
				makeAttempt("ops", 1, 50, fixtureUsage{PromptTokens: 7, CompletionTokens: 11, TotalTokens: 18}, nil, nil),
				makeAttempt("ops", 1, 30, fixtureUsage{}, nil, nil),
				makeAttempt("ops", 1, 70, fixtureUsage{PromptTokens: 13, CompletionTokens: 17, TotalTokens: 30}, nil, nil),
			},
			check: func(t *testing.T, got summaryMetrics) {
				if got.LatencyMinMS != 10 || got.LatencyMeanMS != 50 || got.LatencyP95MS != 90 || got.PromptTokensTotal != 22 || got.CompletionTokensTotal != 31 || got.TotalTokensTotal != 53 || got.TokenUsageAttemptCount != 3 {
					t.Fatalf("metrics=%#v", got)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.check(t, aggregateSummaryMetrics(test.attempts)) })
	}
}

func writeFixture(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeTestFile(t, path, contents)
	return path
}

func assertBoundedPathSegments(t *testing.T, value string) {
	t.Helper()
	for _, segment := range strings.Split(filepath.Clean(value), string(filepath.Separator)) {
		if segment == "" || segment == "." || segment == ".." {
			continue
		}
		limit := len([]byte(segment))
		if limit > maxPOSIXPathSegmentBytes {
			t.Fatalf("path segment too long for POSIX portability: segment=%q bytes=%d", segment, limit)
		}
		if limit > maxPortablePathSegmentBytes {
			t.Fatalf("path segment too long for portable production use: segment=%q bytes=%d", segment, limit)
		}
	}
}
