package query

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/llm"
)

type Receipt struct {
	QueryReceivedAt                 time.Time                 `json:"query_received_at"`
	RunStartedAt                    time.Time                 `json:"run_started_at"`
	RunFinishedAt                   time.Time                 `json:"run_finished_at"`
	ElapsedMS                       int64                     `json:"elapsed_ms"`
	Stages                          []StageReceipt            `json:"stages"`
	HostCalls                       []HostCallReceipt         `json:"host_calls"`
	SelectionLimit                  int                       `json:"selection_limit"`
	ExplorationSlots                int                       `json:"exploration_slots"`
	EvidenceThreshold               int                       `json:"evidence_threshold"`
	ExpansionAttempts               int                       `json:"expansion_attempts"`
	SuccessfulExpansionAttempts     int                       `json:"successful_expansion_attempts"`
	ProviderFailedExpansionAttempts int                       `json:"provider_failed_expansion_attempts"`
	FallbackExpansionCount          int                       `json:"fallback_expansion_count"`
	KeywordsPerExpansionAttempt     int                       `json:"keywords_per_expansion_attempt"`
	RareKeywordMaxDocumentFrequency int                       `json:"rare_keyword_max_document_frequency"`
	KeywordConsensusMinimum         int                       `json:"keyword_consensus_minimum"`
	KeywordSupport                  []KeywordSupportReceipt   `json:"keyword_support,omitempty"`
	ExpansionAttemptOutcomes        []ExpansionAttemptReceipt `json:"expansion_attempt_outcomes,omitempty"`
	RetrievalProfileID              string                    `json:"retrieval_profile_id,omitempty"`
	RetrievalProfileDigest          string                    `json:"retrieval_profile_digest,omitempty"`
	runStartedMono                  time.Time
}

type KeywordSupportReceipt struct {
	Role               string   `json:"role"`
	Kind               string   `json:"-"`
	Value              string   `json:"-"`
	Keyword            string   `json:"-"`
	SurfaceForms       []string `json:"-"`
	KindDigest         string   `json:"kind_digest,omitempty"`
	ValueDigest        string   `json:"value_digest,omitempty"`
	KeywordDigest      string   `json:"keyword_digest,omitempty"`
	SurfaceFormDigests []string `json:"surface_form_digests,omitempty"`
	SupportCount       int      `json:"support_count"`
	AttemptIndexes     []int    `json:"attempt_indexes"`
}

type ExpansionAttemptReceipt struct {
	AttemptIndex int    `json:"attempt_index"`
	Outcome      string `json:"outcome"`
}

func (r *ReceiptRecorder) SetRetrievalConfig(selectionLimit, explorationSlots, evidenceThreshold int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipt.SelectionLimit = selectionLimit
	r.receipt.ExplorationSlots = explorationSlots
	r.receipt.EvidenceThreshold = evidenceThreshold
}

func (r *ReceiptRecorder) SetRetrievalProfile(id, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipt.RetrievalProfileID = id
	r.receipt.RetrievalProfileDigest = digest
}

func (r *ReceiptRecorder) SetExpansionConfig(attempts, successful, providerFailed, keywordsPerAttempt, evidenceThreshold, rareDocumentFrequency, keywordConsensusMinimum int, support []KeywordSupportReceipt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipt.ExpansionAttempts = attempts
	r.receipt.SuccessfulExpansionAttempts = successful
	r.receipt.ProviderFailedExpansionAttempts = providerFailed
	r.receipt.KeywordsPerExpansionAttempt = keywordsPerAttempt
	r.receipt.EvidenceThreshold = evidenceThreshold
	r.receipt.RareKeywordMaxDocumentFrequency = rareDocumentFrequency
	r.receipt.KeywordConsensusMinimum = keywordConsensusMinimum
	r.receipt.KeywordSupport = make([]KeywordSupportReceipt, 0, len(support))
	for _, item := range support {
		item.KindDigest = receiptDigest(item.Kind)
		item.ValueDigest = receiptDigest(item.Value)
		item.KeywordDigest = receiptDigest(item.Keyword)
		item.SurfaceFormDigests = make([]string, 0, len(item.SurfaceForms))
		for _, surface := range item.SurfaceForms {
			item.SurfaceFormDigests = append(item.SurfaceFormDigests, receiptDigest(surface))
		}
		item.Kind, item.Value, item.Keyword, item.SurfaceForms = "", "", "", nil
		r.receipt.KeywordSupport = append(r.receipt.KeywordSupport, item)
	}
}

func receiptDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func (r *ReceiptRecorder) SetFallbackExpansionCount(count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipt.FallbackExpansionCount = count
}

func (r *ReceiptRecorder) SetExpansionAttemptOutcomes(outcomes []ExpansionAttemptReceipt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipt.ExpansionAttemptOutcomes = append([]ExpansionAttemptReceipt(nil), outcomes...)
}

type StageReceipt struct {
	Name        string    `json:"name"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	ElapsedMS   int64     `json:"elapsed_ms"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	Reasoning   string    `json:"reasoning,omitempty"`
	Outcome     string    `json:"outcome"`
	startedMono time.Time
}
type HostCallReceipt struct {
	Sequence    int       `json:"sequence"`
	Stage       string    `json:"stage"`
	Scheme      string    `json:"scheme"`
	Host        string    `json:"host"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	ElapsedMS   int64     `json:"elapsed_ms"`
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Reasoning   string    `json:"reasoning"`
	Outcome     string    `json:"outcome"`
	startedMono time.Time
}

type receiptKey struct{}
type stageContextKey struct{}
type ReceiptRecorder struct {
	mu         sync.Mutex
	receipt    Receipt
	stageIndex map[string]int
	callSeq    int
}

func WithReceipt(ctx context.Context) (context.Context, *ReceiptRecorder) {
	mono := time.Now()
	now := mono.UTC()
	r := &ReceiptRecorder{stageIndex: make(map[string]int), receipt: Receipt{QueryReceivedAt: now, RunStartedAt: now}}
	r.receipt.runStartedMono = mono
	return context.WithValue(llm.WithHostCallRecorder(llm.WithCallRecorder(ctx, r), r), receiptKey{}, r), r
}
func ReceiptRecorderFromContext(ctx context.Context) *ReceiptRecorder {
	r, _ := ctx.Value(receiptKey{}).(*ReceiptRecorder)
	return r
}
func (r *ReceiptRecorder) StartStage(ctx context.Context, name, provider, model, reasoning string) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.receipt.Stages = append(r.receipt.Stages, StageReceipt{Name: name, StartedAt: now.UTC(), startedMono: now, Provider: provider, Model: model, Reasoning: reasoning})
	return context.WithValue(llm.WithCallStage(ctx, name), stageContextKey{}, stageKey{name: name, index: len(r.receipt.Stages) - 1})
}

type stageKey struct {
	name  string
	index int
}

func FinishStage(ctx context.Context, outcome string) {
	r, ok := ctx.Value(receiptKey{}).(*ReceiptRecorder)
	if !ok {
		return
	}
	key, ok := ctx.Value(stageContextKey{}).(stageKey)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &r.receipt.Stages[key.index]
	now := time.Now()
	s.FinishedAt = now.UTC()
	s.ElapsedMS = now.Sub(s.startedMono).Milliseconds()
	s.Outcome = outcome
}
func (r *ReceiptRecorder) StartCall(stageName, model, reasoning string) func(string) {
	return r.StartCallAt(stageName, model, reasoning, "https://api.deepseek.com")
}

func (r *ReceiptRecorder) StartCallAt(stageName, model, reasoning, rawURL string) func(string) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return func(string) {}
	}
	return r.startHostCall(stageName, u.Scheme, u.Hostname(), "deepseek", model, reasoning)
}

func (r *ReceiptRecorder) StartCallAtTimed(stageName, model, reasoning, rawURL string) func(string, time.Time) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return func(string, time.Time) {}
	}
	return r.startHostCallTimed(stageName, u.Scheme, u.Hostname(), "deepseek", model, reasoning)
}

func (r *ReceiptRecorder) StartHostCall(stage, scheme, host string) func(string) {
	return r.startHostCall(stage, scheme, host, "gcs", "", "")
}

func (r *ReceiptRecorder) startHostCall(stageName, scheme, host, provider, model, reasoning string) func(string) {
	finish := r.startHostCallTimed(stageName, scheme, host, provider, model, reasoning)
	return func(outcome string) { finish(outcome, time.Time{}) }
}

func (r *ReceiptRecorder) startHostCallTimed(stageName, scheme, host, provider, model, reasoning string) func(string, time.Time) {
	if net.ParseIP(host) != nil {
		return func(string, time.Time) {}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	r.mu.Lock()
	r.callSeq++
	seq := r.callSeq
	started := time.Now()
	r.mu.Unlock()
	return func(outcome string, finishedAt time.Time) {
		r.mu.Lock()
		defer r.mu.Unlock()
		finished := finishedAt
		if finished.IsZero() {
			finished = time.Now()
		}
		r.receipt.HostCalls = append(r.receipt.HostCalls, HostCallReceipt{Sequence: seq, Stage: stageName, Scheme: scheme, Host: host, StartedAt: started.UTC(), FinishedAt: finished.UTC(), startedMono: started, ElapsedMS: finished.Sub(started).Milliseconds(), Provider: provider, Model: model, Reasoning: reasoning, Outcome: outcome})
	}
}
func FinishReceipt(r *ReceiptRecorder) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.receipt.RunFinishedAt = now.UTC()
	r.receipt.ElapsedMS = now.Sub(r.receipt.runStartedMono).Milliseconds()
	sort.SliceStable(r.receipt.HostCalls, func(i, j int) bool { return r.receipt.HostCalls[i].Sequence < r.receipt.HostCalls[j].Sequence })
	data, _ := json.Marshal(r.receipt)
	log.Printf("query receipt: %s", data)
}
func (r *ReceiptRecorder) Receipt() Receipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	sort.SliceStable(r.receipt.HostCalls, func(i, j int) bool { return r.receipt.HostCalls[i].Sequence < r.receipt.HostCalls[j].Sequence })
	return r.receipt
}
