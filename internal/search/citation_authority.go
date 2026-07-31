package search

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const citationReferencePrefix = "CITATION_REF_"

type citationTarget struct {
	result Result
	rank   int
}

// CitationAuthority owns the exact references issued for one synthesis.
// References are capabilities: only contexts added to this authority can be
// resolved, and each reference maps directly to its trusted result.
type CitationAuthority struct {
	namespace string
	tokens    map[string]citationTarget
	titles    map[string]citationTarget
	ambiguous map[string]struct{}
	issued    []citationTarget
	ranked    []Result
}

// NewCitationAuthority creates a per-synthesis citation capability namespace.
func NewCitationAuthority(ranked ...[]Result) (*CitationAuthority, error) {
	authority, err := newCitationAuthority(rand.Reader)
	if err != nil {
		return nil, err
	}
	if len(ranked) > 0 {
		authority.ranked = append([]Result(nil), ranked[0]...)
	}
	return authority, nil
}

func newCitationAuthority(reader io.Reader) (*CitationAuthority, error) {
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(reader, nonce); err != nil {
		return nil, fmt.Errorf("issue citation capability namespace: %w", err)
	}
	return &CitationAuthority{
		namespace: hex.EncodeToString(nonce),
		tokens:    make(map[string]citationTarget),
		titles:    make(map[string]citationTarget),
		ambiguous: make(map[string]struct{}),
	}, nil
}

// AddContext neutralizes all untrusted context fields before inserting the
// newly issued reference. A context with an unsafe route receives no authority.
func (a *CitationAuthority) AddContext(rank int, result Result, body string) string {
	title := NeutralizeCitationReferences(result.Title)
	slug := NeutralizeCitationReferences(result.Slug)
	body = NeutralizeCitationReferences(body)

	reference := ""
	if safeCitationResult(result) {
		reference = a.reference(rank)
		target := citationTarget{result: result, rank: rank}
		a.tokens[reference] = target
		a.issued = append(a.issued, target)
		if _, exists := a.ambiguous[title]; !exists {
			if _, exists := a.titles[title]; exists {
				delete(a.titles, title)
				a.ambiguous[title] = struct{}{}
			} else {
				a.titles[title] = target
			}
		}
	}

	var context strings.Builder
	context.Grow(len(title) + len(slug) + len(body) + len(reference) + 8)
	context.WriteByte('[')
	context.WriteString(title)
	context.WriteByte(']')
	if reference != "" {
		context.WriteByte(' ')
		context.WriteString(reference)
	}
	if slug != "" {
		context.WriteByte(' ')
		context.WriteString(slug)
	}
	context.WriteString("\n\n")
	context.WriteString(body)
	return context.String()
}

func (a *CitationAuthority) reference(rank int) string {
	return "[" + citationReferencePrefix + a.namespace + "_" + strconv.Itoa(rank) + "]"
}

// Resolve accepts only exact capabilities and unique byte-for-byte canonical
// titles issued by this authority. It preserves ordinary answer text and
// neutralizes reserved-looking text in one forward pass.
func (a *CitationAuthority) Resolve(answer string) (string, []Citation, []Result) {
	if answer == "" {
		return answer, nil, a.authorizedResults(nil)
	}

	var normalized strings.Builder
	normalized.Grow(len(answer))
	var citations []Citation
	cited := make(map[string]struct{})

	for offset := 0; offset < len(answer); {
		open := strings.IndexByte(answer[offset:], '[')
		if open < 0 {
			writeNeutralized(&normalized, answer[offset:])
			break
		}
		open += offset
		writeNeutralized(&normalized, answer[offset:open])

		closeOffset := strings.IndexByte(answer[open+1:], ']')
		if closeOffset < 0 {
			writeNeutralized(&normalized, answer[open:])
			break
		}
		close := open + 1 + closeOffset
		bracketed := answer[open : close+1]
		inside := answer[open+1 : close]
		if target, ok := a.tokens[bracketed]; ok {
			appendCitation(&normalized, &citations, cited, target)
		} else if target, ok := a.titles[inside]; ok {
			appendCitation(&normalized, &citations, cited, target)
		} else {
			writeNeutralized(&normalized, bracketed)
		}
		offset = close + 1
	}

	if len(citations) == 0 {
		return normalized.String(), nil, a.authorizedResults(nil)
	}
	return normalized.String(), citations, a.authorizedResults(cited)
}

func appendCitation(normalized *strings.Builder, citations *[]Citation, cited map[string]struct{}, target citationTarget) {
	result := target.result
	title := NeutralizeCitationReferences(result.Title)
	collection := "concepts"
	if result.Type == "source" {
		collection = "sources"
	}
	path := "/" + collection + "/" + url.PathEscape(result.Slug)
	*citations = append(*citations, Citation{Text: title, Slug: result.Slug, Type: result.Type, Path: path})
	cited[result.Type+"\x00"+result.Slug] = struct{}{}
	normalized.WriteByte('[')
	normalized.WriteString(title)
	normalized.WriteByte(']')
}

func (a *CitationAuthority) authorizedResults(cited map[string]struct{}) []Result {
	if cited == nil {
		return append([]Result(nil), a.ranked...)
	}
	if len(a.tokens) == 0 {
		return nil
	}
	targets := make([]citationTarget, 0, len(a.issued))
	seen := make(map[string]struct{}, len(a.issued))
	for _, target := range a.issued {
		identity := target.result.Type + "\x00" + target.result.Slug
		if cited != nil {
			if _, ok := cited[identity]; !ok {
				continue
			}
		}
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		targets = append(targets, target)
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].rank < targets[j].rank })
	results := make([]Result, len(targets))
	for i, target := range targets {
		results[i] = target.result
	}
	return results
}

// NeutralizeCitationReferences makes reserved-looking untrusted text harmless
// while preserving the rest of the text byte-for-byte.
func NeutralizeCitationReferences(text string) string {
	return strings.ReplaceAll(text, citationReferencePrefix, "CITATION-REF_")
}

func writeNeutralized(builder *strings.Builder, text string) {
	if strings.Contains(text, citationReferencePrefix) {
		builder.WriteString(NeutralizeCitationReferences(text))
		return
	}
	builder.WriteString(text)
}

func safeCitationResult(result Result) bool {
	if result.Type != "source" && result.Type != "concept" {
		return false
	}
	if result.Slug == "" || strings.TrimSpace(result.Slug) != result.Slug || result.Slug == "." || result.Slug == ".." {
		return false
	}
	if strings.ContainsAny(result.Slug, "/\\%?#") || strings.HasPrefix(result.Slug, "//") {
		return false
	}
	for _, r := range result.Slug {
		if r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			return false
		}
	}
	parsed, err := url.Parse(result.Slug)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return false
	}
	escaped := url.PathEscape(result.Slug)
	unescaped, err := url.PathUnescape(escaped)
	return escaped != "" && escaped != "." && escaped != ".." && err == nil && unescaped == result.Slug && !strings.Contains(escaped, "/")
}
