package query

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/llm"
)

type Receipt struct {
	QueryReceivedAt   time.Time         `json:"query_received_at"`
	RunStartedAt      time.Time         `json:"run_started_at"`
	RunFinishedAt     time.Time         `json:"run_finished_at"`
	ElapsedMS         int64             `json:"elapsed_ms"`
	Stages            []StageReceipt    `json:"stages"`
	HostCalls         []HostCallReceipt `json:"host_calls"`
	SelectionLimit    int               `json:"selection_limit"`
	ExplorationSlots  int               `json:"exploration_slots"`
	EvidenceThreshold int               `json:"evidence_threshold"`
	runStartedMono    time.Time
}

func (r *ReceiptRecorder) SetRetrievalConfig(selectionLimit, explorationSlots, evidenceThreshold int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipt.SelectionLimit = selectionLimit
	r.receipt.ExplorationSlots = explorationSlots
	r.receipt.EvidenceThreshold = evidenceThreshold
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
	data, _ := json.Marshal(r.receipt)
	log.Printf("query receipt: %s", data)
}
func (r *ReceiptRecorder) Receipt() Receipt { r.mu.Lock(); defer r.mu.Unlock(); return r.receipt }
