package queryquality_test

import (
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/queryquality"
)

func TestBuiltInPromptCatalogHasLifestyleAndTechnicalIdentities(t *testing.T) {
	lifestyle, ok := queryquality.LookupPrompt(queryquality.StructuredPlanPromptID)
	if !ok || lifestyle.TemplateDigest == "" {
		t.Fatalf("lifestyle prompt = %#v, ok=%v", lifestyle, ok)
	}
	technical, ok := queryquality.LookupPrompt("domain-neutral-technical-v1")
	if !ok || technical.TemplateDigest == "" || technical.ID == lifestyle.ID {
		t.Fatalf("technical prompt = %#v, ok=%v", technical, ok)
	}
	if err := queryquality.ValidatePromptTemplate(technical.ID, "wrong", "wrong"); err == nil {
		t.Fatal("accepted unsupported template content")
	}
	rendered, err := queryquality.RenderPrompt(technical.ID, "How does TLS work?", queryquality.DefaultCriterionPolicy, 24)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered.System, "wrong") || !strings.Contains(rendered.User, "How does TLS work?") {
		t.Fatalf("rendered prompt = %#v", rendered)
	}
}

func TestRenderPromptReplacesPlaceholdersOnce(t *testing.T) {
	query := "literal {{criterion_policy}}"
	policy := queryquality.CriterionPolicy{RequiredWhenExplicit: []string{"literal {{raw_query}}"}}
	rendered, err := queryquality.RenderPrompt(queryquality.StructuredPlanPromptID, query, policy, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.User, query) || !strings.Contains(rendered.User, `literal {{raw_query}}`) {
		t.Fatalf("rendered prompt rewrote inserted placeholders: %q", rendered.User)
	}
}
