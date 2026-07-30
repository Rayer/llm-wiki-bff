package handler

import (
	"strings"
	"testing"
)

func TestQueryPromptsRequireExactInternalReferencesInBothModes(t *testing.T) {
	for _, mode := range []string{"wiki", "full"} {
		prompt := buildSystemPrompt(mode)
		if !strings.Contains(prompt, "[CITATION_REF_0]") || !strings.Contains(prompt, "Use that exact reference") {
			t.Fatalf("%s prompt lost exact internal-reference rules: %q", mode, prompt)
		}
	}
}
