package queryquality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const DomainNeutralTechnicalPromptID = "domain-neutral-technical-v1"

// PromptTemplateDigest hashes the compact JSON array [system_template,user_template].
// The array is the documented canonical pair; JSON escaping is therefore part of
// the immutable prompt identity and the digest is always the full SHA-256 value.
func PromptTemplateDigest(systemTemplate, userTemplate string) string {
	data, _ := json.Marshal([2]string{systemTemplate, userTemplate})
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

type PromptIdentity struct {
	ID             string
	TemplateDigest string
}

type RenderedPrompt struct {
	System string
	User   string
}

type builtInPrompt struct {
	PromptIdentity
	systemTemplate string
	userTemplate   string
	jsonRawQuery   bool
}

const (
	StructuredPlanPromptID     = "minimal-v1"
	structuredPlanSystemPrompt = `You produce a retrieval plan for a frozen Lifestyle concept corpus. Return exactly one JSON object and no markdown. The object fields and exact types are: raw_query string; required array of Criterion; excluded array of Criterion; preferred array of Criterion; goals array of Criterion; supporting_dimensions array of Criterion; acceptable_alternatives array of Criterion; ambiguity array of strings; fallback boolean. Every Criterion is exactly {kind:string,value:string,terms:array of strings,proof:"lexical" or "semantic"}. Every lexical Criterion needs at least one discovery term. Never output a string where an array or object is required. Be conservative: only explicit user constraints may be required or excluded; absent never means excluded. In this minimal variant, supporting_dimensions and acceptable_alternatives must be empty arrays and fallback must be false.`
	structuredPlanUserTemplate = "Raw query: {{raw_query}}\nCriterion policy: {{criterion_policy}}\nInterpret the query into required, excluded, preferred and goals. Preserve the raw query exactly in raw_query. Return the single JSON object only."
	technicalSystemTemplate    = `You produce a retrieval plan for a frozen heterogeneous technical and documentation concept corpus. Return exactly one JSON object and no markdown. The object fields and exact types are: raw_query string; required array of Criterion; excluded array of Criterion; preferred array of Criterion; goals array of Criterion; supporting_dimensions array of Criterion; acceptable_alternatives array of Criterion; ambiguity array of strings; fallback boolean. Every Criterion is exactly {kind:string,value:string,terms:array of strings,proof:"lexical" or "semantic"}. Every lexical Criterion needs at least one discovery term. Never output a string where an array or object is required. Be conservative: only explicit user hard constraints may be required; only explicit negative language may be excluded or use an exclusion criterion; absent never means excluded. Named technologies, components, services, processes, mechanisms, and document topics are positive entities or relations, not locations, venues, audiences, or exclusions unless the raw query explicitly says so. Multi-entity explanatory questions may distribute evidence across multiple Concepts; do not require every candidate Concept to contain every named entity and relation. In this minimal variant, supporting_dimensions and acceptable_alternatives must be empty arrays and fallback must be false.`
	technicalUserTemplate      = "Raw query: {{raw_query}}\nCriterion policy: {{criterion_policy}}\nInterpret the query into required, excluded, preferred and goals for technical/document retrieval. Preserve the raw query exactly in raw_query. Return the single JSON object only."
)

var builtInPrompts = func() map[string]builtInPrompt {
	result := map[string]builtInPrompt{
		StructuredPlanPromptID: {
			PromptIdentity: PromptIdentity{ID: StructuredPlanPromptID, TemplateDigest: PromptTemplateDigest(structuredPlanSystemPrompt, structuredPlanUserTemplate)},
			systemTemplate: structuredPlanSystemPrompt,
			userTemplate:   structuredPlanUserTemplate,
			jsonRawQuery:   true,
		},
		DomainNeutralTechnicalPromptID: {
			PromptIdentity: PromptIdentity{ID: DomainNeutralTechnicalPromptID, TemplateDigest: PromptTemplateDigest(technicalSystemTemplate, technicalUserTemplate)},
			systemTemplate: technicalSystemTemplate,
			userTemplate:   technicalUserTemplate,
			jsonRawQuery:   false,
		},
	}
	return result
}()

func LookupPrompt(id string) (PromptIdentity, bool) {
	prompt, ok := builtInPrompts[id]
	return prompt.PromptIdentity, ok
}

func ValidatePrompt(id, digest string) error {
	prompt, ok := builtInPrompts[id]
	if !ok {
		return fmt.Errorf("unsupported prompt id %q", id)
	}
	if digest != prompt.TemplateDigest {
		return errors.New("prompt template digest mismatch")
	}
	return nil
}

func ValidatePromptTemplate(id, systemTemplate, userTemplate string) error {
	prompt, ok := builtInPrompts[id]
	if !ok {
		return fmt.Errorf("unsupported prompt id %q", id)
	}
	if systemTemplate != prompt.systemTemplate || userTemplate != prompt.userTemplate {
		return errors.New("prompt template content mismatch")
	}
	return nil
}

func RenderPrompt(id, rawQuery string, policy CriterionPolicy, keywordsPerAttempt int) (RenderedPrompt, error) {
	prompt, ok := builtInPrompts[id]
	if !ok {
		return RenderedPrompt{}, fmt.Errorf("unsupported prompt id %q", id)
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return RenderedPrompt{}, err
	}
	raw := rawQuery
	if prompt.jsonRawQuery {
		encoded, _ := json.Marshal(rawQuery)
		raw = string(encoded)
	}
	replace := strings.NewReplacer("{{raw_query}}", raw, "{{criterion_policy}}", string(policyJSON)).Replace
	user := replace(prompt.userTemplate)
	if keywordsPerAttempt > 0 {
		user += fmt.Sprintf("\nMaximum normalized positive discovery keywords for this attempt: %d.", keywordsPerAttempt)
	}
	return RenderedPrompt{System: replace(prompt.systemTemplate), User: user}, nil
}

// LookupPromptTemplate returns the immutable production template pair.
func LookupPromptTemplate(id string) (system, user string, ok bool) {
	prompt, ok := builtInPrompts[id]
	if !ok {
		return "", "", false
	}
	return prompt.systemTemplate, prompt.userTemplate, true
}

func renderPromptJSONQuery(id, rawQuery string, policy CriterionPolicy, keywordsPerAttempt int) (RenderedPrompt, error) {
	return RenderPrompt(id, rawQuery, policy, keywordsPerAttempt)
}
