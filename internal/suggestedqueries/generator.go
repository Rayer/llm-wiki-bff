package suggestedqueries

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/generation"
)

const (
	MinQueries = 3
	// MaxLegacyQueries is the largest readable v2 artifact from the pre-exact-20
	// publishing contract. Newly generated artifacts use RequiredQueries exactly.
	MaxLegacyQueries = 5
	RequiredQueries  = 20
	MaxConcepts      = 12
	MaxQuestionBytes = 512
	MaxProviderBytes = 64 * 1024
	PromptVersion    = "lwc-249-v1"
	maxWrapperRunes  = 512
)

var (
	ErrInvalidCandidates = errors.New("invalid suggested-query candidates")
)

// ConceptEvidence is the bounded corpus evidence supplied to generation and
// used to validate candidate anchor IDs.
type ConceptEvidence struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
}

// GenerationMetadata identifies the generation contract that produced a
// candidate. It is retained in the published artifact for auditability.
type GenerationMetadata struct {
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
}

// Candidate is the validated candidate retained in the published artifact.
// Generation is attached locally after provider output validation.
type Candidate struct {
	Question               string             `json:"question"`
	Intent                 string             `json:"intent/use_case"`
	CorpusAnchorConceptIDs []string           `json:"corpus_anchor_concept_ids"`
	Generation             GenerationMetadata `json:"generation"`
}

type providerCandidate struct {
	Question               string
	Intent                 string
	CorpusAnchorConceptIDs []string
}

type Provider interface {
	Chat(ctx context.Context, systemPrompt, userMessage string) (string, error)
}

// Generate makes one bounded provider call for one postprocess operation.
func Generate(ctx context.Context, provider Provider, description string, entries []conceptcache.Entry, mtimes map[string]time.Time, trustedGeneration GenerationMetadata, now time.Time) (Artifact, error) {
	if provider == nil {
		return Artifact{}, errors.New("suggested-query provider is not configured")
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	concepts := RepresentativeConcepts(entries, mtimes)
	if len(concepts) == 0 {
		return Artifact{}, fmt.Errorf("%w: no corpus concepts", ErrInvalidCandidates)
	}
	if strings.TrimSpace(trustedGeneration.Model) == "" || strings.TrimSpace(trustedGeneration.PromptVersion) == "" {
		return Artifact{}, fmt.Errorf("%w: trusted generation metadata is incomplete", ErrInvalidCandidates)
	}
	description = truncateBytes(strings.TrimSpace(description), 2048)
	input := struct {
		Description string            `json:"project_description,omitempty"`
		Concepts    []ConceptEvidence `json:"concepts"`
	}{Description: description, Concepts: concepts}
	userData, err := json.Marshal(input)
	if err != nil {
		return Artifact{}, fmt.Errorf("marshal generation input: %w", err)
	}
	raw, err := provider.Chat(ctx, generationSystemPrompt, string(userData))
	if err != nil {
		return Artifact{}, fmt.Errorf("generate suggested queries: %w", err)
	}
	candidates, err := parseProviderCandidates(raw)
	if err != nil {
		return Artifact{}, err
	}
	if err := validateGeneratedCandidates(candidates, concepts); err != nil {
		return Artifact{}, err
	}
	for i := range candidates {
		candidates[i].Generation = trustedGeneration
	}
	return ArtifactFromCandidates(candidates, now), nil
}

const generationSystemPrompt = `Generate natural-language discovery questions for the supplied corpus.
Return only a JSON object with a candidates array. Each candidate must contain question,
intent/use_case, and corpus_anchor_concept_ids. Do not include generation metadata.
Use only supplied concept IDs as corpus anchors. Use-case hypotheses are allowed only as
questions to be decided by retrieval; do not assert unsupported attributes as facts.
Return exactly 20 distinct questions. Never return a bare title or a trivial title wrapper.`

func RepresentativeConcepts(entries []conceptcache.Entry, mtimes map[string]time.Time) []ConceptEvidence {
	type ranked struct {
		entry conceptcache.Entry
		when  time.Time
		order int
	}
	rankedEntries := make([]ranked, 0, len(entries))
	for i, entry := range entries {
		if isSystemMetaConcept(entry) || strings.TrimSpace(entry.Title) == "" {
			continue
		}
		rankedEntries = append(rankedEntries, ranked{entry: entry, when: conceptUpdatedAt(entry, mtimes, i), order: i})
	}
	sort.SliceStable(rankedEntries, func(i, j int) bool {
		if rankedEntries[i].when.Equal(rankedEntries[j].when) {
			return rankedEntries[i].order < rankedEntries[j].order
		}
		return rankedEntries[i].when.After(rankedEntries[j].when)
	})
	if len(rankedEntries) > MaxConcepts {
		rankedEntries = rankedEntries[:MaxConcepts]
	}
	result := make([]ConceptEvidence, 0, len(rankedEntries))
	for _, item := range rankedEntries {
		id := strings.TrimSpace(frontmatterString(item.entry.Frontmatter["id"]))
		if id == "" {
			id = strings.TrimSpace(item.entry.Slug)
		}
		if id == "" {
			continue
		}
		evidence := strings.TrimSpace(item.entry.Body)
		if evidence == "" {
			evidence = strings.TrimSpace(item.entry.Title)
		}
		result = append(result, ConceptEvidence{
			ID:       id,
			Title:    truncateBytes(strings.TrimSpace(item.entry.Title), 256),
			Evidence: truncateBytes(evidence, 1200),
		})
	}
	return result
}

func isSystemMetaConcept(entry conceptcache.Entry) bool {
	slug := strings.Trim(strings.ToLower(strings.TrimSpace(entry.Slug)), "/")
	if slug == "index" || slug == "log" {
		return true
	}
	for _, key := range []string{"path", "source_file"} {
		value := strings.Trim(strings.ToLower(frontmatterString(entry.Frontmatter[key])), "/")
		if value == "wiki/index.md" || value == "wiki/log.md" {
			return true
		}
	}
	return false
}

func frontmatterString(value interface{}) string {
	text, _ := value.(string)
	return text
}

func parseProviderCandidates(raw string) ([]Candidate, error) {
	if len(raw) > MaxProviderBytes {
		return nil, errors.New("suggested-query provider output exceeds byte limit")
	}
	// DeepSeek (and other chat models) often wrap JSON in markdown fences despite
	// "return only JSON" instructions. Strip the same way query expander does.
	raw = stripMarkdownCodeFence(raw)
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	providerCandidates, err := decodeProviderEnvelope(dec)
	if err != nil {
		return nil, fmt.Errorf("decode suggested-query provider output: %w", err)
	}
	if err := generation.EnsureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("decode suggested-query provider output: %w", err)
	}
	if len(providerCandidates) > MaxQueries {
		return nil, fmt.Errorf("%w: candidate count exceeds %d", ErrInvalidCandidates, MaxQueries)
	}
	candidates := make([]Candidate, len(providerCandidates))
	for i, candidate := range providerCandidates {
		candidates[i] = Candidate{
			Question:               candidate.Question,
			Intent:                 candidate.Intent,
			CorpusAnchorConceptIDs: candidate.CorpusAnchorConceptIDs,
		}
	}
	return candidates, nil
}

func decodeProviderEnvelope(dec *json.Decoder) ([]providerCandidate, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("expected provider envelope object")
	}
	seen := make(map[string]struct{}, 1)
	var candidates []providerCandidate
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("expected provider envelope key")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate provider envelope key %q", name)
		}
		seen[name] = struct{}{}
		if name != "candidates" {
			return nil, fmt.Errorf("unknown provider envelope key %q", name)
		}
		candidates, err = decodeProviderCandidateArray(dec)
		if err != nil {
			return nil, err
		}
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim('}') {
		return nil, errors.New("expected provider envelope object end")
	}
	return candidates, nil
}

func decodeProviderCandidateArray(dec *json.Decoder) ([]providerCandidate, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, errors.New("expected provider candidates array")
	}
	candidates := make([]providerCandidate, 0, MinQueries)
	for dec.More() {
		if len(candidates) >= MaxQueries {
			return nil, fmt.Errorf("%w: candidate count exceeds %d", ErrInvalidCandidates, MaxQueries)
		}
		candidate, err := decodeProviderCandidate(dec)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim(']') {
		return nil, errors.New("expected provider candidates array end")
	}
	return candidates, nil
}

func decodeProviderCandidate(dec *json.Decoder) (providerCandidate, error) {
	token, err := dec.Token()
	if err != nil {
		return providerCandidate{}, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return providerCandidate{}, errors.New("expected provider candidate object")
	}
	var candidate providerCandidate
	seen := make(map[string]struct{}, 3)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return providerCandidate{}, err
		}
		name, ok := key.(string)
		if !ok {
			return providerCandidate{}, errors.New("expected provider candidate key")
		}
		if _, duplicate := seen[name]; duplicate {
			return providerCandidate{}, fmt.Errorf("duplicate provider candidate key %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "question":
			candidate.Question, err = decodeJSONString(dec)
		case "intent/use_case":
			candidate.Intent, err = decodeJSONString(dec)
		case "corpus_anchor_concept_ids":
			candidate.CorpusAnchorConceptIDs, err = decodeAnchorIDs(dec)
		default:
			return providerCandidate{}, fmt.Errorf("unknown provider candidate key %q", name)
		}
		if err != nil {
			return providerCandidate{}, err
		}
	}
	end, err := dec.Token()
	if err != nil {
		return providerCandidate{}, err
	}
	if end != json.Delim('}') {
		return providerCandidate{}, errors.New("expected provider candidate object end")
	}
	return candidate, nil
}

func decodeAnchorIDs(dec *json.Decoder) ([]string, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, errors.New("expected corpus anchor array")
	}
	ids := make([]string, 0, 1)
	for dec.More() {
		if len(ids) >= MaxConcepts {
			return nil, fmt.Errorf("%w: anchor count exceeds %d", ErrInvalidCandidates, MaxConcepts)
		}
		id, err := decodeJSONString(dec)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim(']') {
		return nil, errors.New("expected corpus anchor array end")
	}
	return ids, nil
}

func ArtifactFromCandidates(candidates []Candidate, now time.Time) Artifact {
	queries := make([]string, len(candidates))
	for i, candidate := range candidates {
		queries[i] = strings.TrimSpace(candidate.Question)
	}
	return Artifact{Version: 2, Queries: queries, Candidates: append([]Candidate(nil), candidates...), UpdatedAt: now.UTC().Format(time.RFC3339)}
}

func IsPublishable(artifact Artifact) bool {
	count := len(artifact.Candidates)
	if artifact.Version != 2 || !((count >= MinQueries && count <= MaxLegacyQueries) || count == RequiredQueries) || len(artifact.Queries) != count {
		return false
	}
	if err := validateCandidates(artifact.Candidates, nil, false, true); err != nil {
		return false
	}
	for i, candidate := range artifact.Candidates {
		if strings.TrimSpace(artifact.Queries[i]) != strings.TrimSpace(candidate.Question) {
			return false
		}
	}
	return true
}

func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for len(value) > limit || !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func ValidateCandidates(candidates []Candidate, concepts []ConceptEvidence) error {
	return validateCandidates(candidates, concepts, true, true)
}

func validateGeneratedCandidates(candidates []Candidate, concepts []ConceptEvidence) error {
	if len(candidates) != RequiredQueries {
		return fmt.Errorf("%w: candidate count %d, want exactly %d", ErrInvalidCandidates, len(candidates), RequiredQueries)
	}
	return validateCandidates(candidates, concepts, true, false)
}

func validateCandidates(candidates []Candidate, concepts []ConceptEvidence, checkAnchors, requireGeneration bool) error {
	// This validator proves structure, question framing, and corpus-anchor IDs;
	// it cannot prove arbitrary semantic attributes. Those remain provider
	// hypotheses and are safe only when phrased as questions.
	if len(candidates) < MinQueries || len(candidates) > MaxQueries {
		return fmt.Errorf("%w: candidate count %d outside %d..%d", ErrInvalidCandidates, len(candidates), MinQueries, MaxQueries)
	}
	titles := make(map[string]string, len(concepts))
	knownIDs := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		if id := strings.TrimSpace(concept.ID); id != "" {
			knownIDs[id] = struct{}{}
		}
		if title := normalize(concept.Title); title != "" {
			titles[title] = strings.TrimSpace(concept.Title)
		}
	}
	seen := make(map[string]struct{}, len(candidates))
	for i, candidate := range candidates {
		question := strings.TrimSpace(candidate.Question)
		if question == "" || len([]byte(question)) > MaxQuestionBytes {
			return fmt.Errorf("%w: candidate %d question is empty or oversized", ErrInvalidCandidates, i)
		}
		questionKey := normalize(question)
		if questionKey == "" || strings.ContainsAny(question, "?？") == false {
			return fmt.Errorf("%w: candidate %d is not a question", ErrInvalidCandidates, i)
		}
		if _, exists := seen[questionKey]; exists {
			return fmt.Errorf("%w: duplicate question", ErrInvalidCandidates)
		}
		seen[questionKey] = struct{}{}
		if _, exactTitle := titles[questionKey]; exactTitle || isTitleWrapper(question, titles) {
			return fmt.Errorf("%w: candidate %d is title-like", ErrInvalidCandidates, i)
		}
		if strings.TrimSpace(candidate.Intent) == "" {
			return fmt.Errorf("%w: candidate %d metadata is incomplete", ErrInvalidCandidates, i)
		}
		if requireGeneration && (strings.TrimSpace(candidate.Generation.Model) == "" || strings.TrimSpace(candidate.Generation.PromptVersion) == "") {
			return fmt.Errorf("%w: candidate %d generation metadata is incomplete", ErrInvalidCandidates, i)
		}
		if len(candidate.CorpusAnchorConceptIDs) == 0 || len(candidate.CorpusAnchorConceptIDs) > MaxConcepts {
			return fmt.Errorf("%w: candidate %d anchors are missing or oversized", ErrInvalidCandidates, i)
		}
		anchorSeen := make(map[string]struct{}, len(candidate.CorpusAnchorConceptIDs))
		for _, rawID := range candidate.CorpusAnchorConceptIDs {
			id := strings.TrimSpace(rawID)
			if id == "" {
				return fmt.Errorf("%w: candidate %d has empty anchor", ErrInvalidCandidates, i)
			}
			if _, duplicate := anchorSeen[id]; duplicate {
				return fmt.Errorf("%w: candidate %d has duplicate anchor", ErrInvalidCandidates, i)
			}
			anchorSeen[id] = struct{}{}
			if checkAnchors {
				if _, known := knownIDs[id]; !known {
					return fmt.Errorf("%w: candidate %d has unsupported anchor %q", ErrInvalidCandidates, i, id)
				}
			}
		}
	}
	return nil
}

func isTitleWrapper(question string, titles map[string]string) bool {
	if utf8.RuneCountInString(question) > maxWrapperRunes {
		return false
	}

	questionTokens := tokenizeWrapperWords(question)
	titleSequences := make(map[string]struct{}, len(titles))
	for _, title := range titles {
		if utf8.RuneCountInString(title) > maxWrapperRunes {
			continue
		}
		tokens := tokenizeWrapperWords(title)
		if len(tokens) > 0 && len(tokens) <= len(questionTokens) {
			titleSequences[wrapperTokenKey(tokens)] = struct{}{}
		}
	}
	if len(titleSequences) == 0 {
		return false
	}

	anchored := make([]bool, len(questionTokens))
	anchorFound := false
	for start := range questionTokens {
		for end := start + 1; end <= len(questionTokens); end++ {
			if _, ok := titleSequences[wrapperTokenKey(questionTokens[start:end])]; !ok {
				continue
			}
			anchorFound = true
			for i := start; i < end; i++ {
				anchored[i] = true
			}
		}
	}
	if !anchorFound {
		return false
	}

	residualTokens := make([]wrapperToken, 0, len(questionTokens))
	for i, token := range questionTokens {
		if !anchored[i] {
			residualTokens = append(residualTokens, token)
		}
	}
	return stripGenericWrapperLanguage(wrapperTokensText(residualTokens)) == ""
}

var genericWrapperEnglishTokens = map[string]struct{}{
	"a": {}, "about": {}, "all": {}, "an": {}, "are": {}, "available": {}, "can": {}, "content": {},
	"could": {}, "detail": {}, "details": {}, "define": {}, "definition": {}, "description": {}, "describe": {}, "discuss": {},
	"elaborate": {}, "explain": {}, "for": {}, "give": {}, "how": {}, "information": {}, "introduction": {}, "tell": {},
	"is": {}, "me": {}, "of": {}, "on": {}, "outline": {}, "overview": {}, "please": {}, "provide": {}, "rundown": {},
	"show": {}, "summary": {}, "summarize": {}, "the": {}, "there": {}, "this": {}, "to": {}, "topic": {},
	"what": {}, "which": {}, "with": {}, "you": {},
}

var genericWrapperPhrases = []string{
	// English glue is already represented by exact token removal; these phrases keep
	// bounded cross-token wrappers.
	"can you",
	"could you",
	"please",
	"what is",
	"what's",

	// Bounded Chinese polite, topic, content, information, and explanation phrases.
	"請告訴我", "請問", "請提供", "我想知道", "想了解", "可以介紹一下", "可以說明", "介紹", "說明", "提供", "關於", "所有", "相關", "完整資訊", "完整內容", "資訊", "資料", "詳情", "這個主題", "這個概念", "主題", "有哪些", "內容", "哪個", "哪些", "有什麼", "可以", "嗎", "呢", "的", "和",
	"請", "描述", "概述", "摘要", "簡介", "總結", "解釋",
}

type wrapperToken struct {
	text string
}

func tokenizeWrapperWords(value string) []wrapperToken {
	runes := []rune(strings.ToLower(value))
	tokens := make([]wrapperToken, 0, len(runes))
	for i := 0; i < len(runes); {
		r := runes[i]
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			i++
			continue
		}
		if unicode.Is(unicode.Han, r) {
			tokens = append(tokens, wrapperToken{text: string(r)})
			i++
			continue
		}
		start := i
		for i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsNumber(runes[i])) && !unicode.Is(unicode.Han, runes[i]) {
			i++
		}
		tokens = append(tokens, wrapperToken{text: string(runes[start:i])})
	}
	return tokens
}

func wrapperTokenKey(tokens []wrapperToken) string {
	values := make([]string, len(tokens))
	for i, token := range tokens {
		values[i] = token.text
	}
	return strings.Join(values, "\x00")
}

func wrapperTokensText(tokens []wrapperToken) string {
	values := make([]string, len(tokens))
	for i, token := range tokens {
		values[i] = token.text
	}
	return strings.Join(values, " ")
}

func stripGenericWrapperLanguage(value string) string {
	tokens := tokenizeWrapperWords(value)
	phrases := make([][]wrapperToken, 0, len(genericWrapperPhrases))
	for _, phrase := range genericWrapperPhrases {
		if phraseTokens := tokenizeWrapperWords(phrase); len(phraseTokens) > 0 {
			phrases = append(phrases, phraseTokens)
		}
	}

	kept := make([]wrapperToken, 0, len(tokens))
	for i := 0; i < len(tokens); {
		if _, ok := genericWrapperEnglishTokens[tokens[i].text]; ok {
			i++
			continue
		}
		matched := 0
		for _, phrase := range phrases {
			if len(phrase) <= matched || i+len(phrase) > len(tokens) {
				continue
			}
			match := true
			for j := range phrase {
				if phrase[j].text != tokens[i+j].text {
					match = false
					break
				}
			}
			if match {
				matched = len(phrase)
			}
		}
		if matched > 0 {
			i += matched
			continue
		}
		kept = append(kept, tokens[i])
		i++
	}

	// This is a deterministic Chinese/English generic-wrapper guard, not a
	// universal semantic proof. It is deliberately bounded by the caller's
	// question size and uses only exact tokens/phrases, with no domain lexicon.
	return wrapperTokensText(kept)
}

func normalize(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripMarkdownCodeFence removes a single leading/trailing ``` fence wrapper
// (optionally with a language tag such as ```json). Unfenced input is returned
// trimmed. This matches internal/llm/query_expander behavior.
func stripMarkdownCodeFence(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "```") {
		return raw
	}
	lines := strings.SplitN(raw, "\n", 3)
	if len(lines) < 2 {
		return raw
	}
	raw = strings.TrimPrefix(raw, lines[0]+"\n")
	raw = strings.TrimSuffix(raw, "\n```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}
