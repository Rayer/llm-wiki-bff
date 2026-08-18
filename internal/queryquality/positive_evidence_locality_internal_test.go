package queryquality

import "testing"

func TestPositiveEvidenceDimensionsHelperUsesRoleLocalMatching(t *testing.T) {
	plan := QueryPlan{
		Preferred:            []Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"cafe"}, Proof: "lexical"}},
		SupportingDimensions: []Criterion{{Kind: "topic", Value: "coffee", Terms: []string{"coffee"}, Proof: "lexical"}},
	}
	groups := []GroupEvidence{
		{Role: "supporting", Kind: "topic", Value: "coffee", Matches: []FieldEvidence{{Field: "body", Terms: []string{"coffee"}}}},
	}

	if hasMatchedGroup(groups, "preferred", plan.Preferred[0]) {
		t.Fatal("preferred criterion should not match supporting group")
	}
	if !hasMatchedGroup(groups, "supporting", plan.SupportingDimensions[0]) {
		t.Fatal("supporting criterion should match supporting group")
	}

	dimensions := positiveEvidenceDimensions(plan, groups)
	if len(dimensions) != 1 || dimensions[0] != "topic" {
		t.Fatalf("positiveEvidenceDimensions() = %#v, want one topic dimension", dimensions)
	}
}
