package jsonutil

import (
	"encoding/json"
	"testing"
)

func TestNormalizeNestedYAMLMapsAreJSONSafe(t *testing.T) {
	input := map[string]interface{}{
		"id": "concept-id",
		"lineage": []interface{}{
			map[interface{}]interface{}{
				"operation": "merge",
				"metadata": map[interface{}]interface{}{
					"reason": "rename",
				},
			},
		},
	}

	got, err := NormalizeMap(input)
	if err != nil {
		t.Fatalf("NormalizeMap() error = %v", err)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	lineage := decoded["lineage"].([]interface{})
	entry := lineage[0].(map[string]interface{})
	if entry["operation"] != "merge" {
		t.Fatalf("operation = %#v", entry["operation"])
	}
	if entry["metadata"].(map[string]interface{})["reason"] != "rename" {
		t.Fatalf("metadata = %#v", entry["metadata"])
	}
}

func TestNormalizeRejectsNonStringMapKeys(t *testing.T) {
	_, err := Normalize(map[interface{}]interface{}{1: "value"}, 0)
	if err == nil {
		t.Fatal("Normalize() error = nil, want non-string map key rejection")
	}
}

func TestNormalizeRejectsExcessiveDepth(t *testing.T) {
	var value interface{} = "leaf"
	for i := 0; i <= MaxNormalizationDepth; i++ {
		value = map[string]interface{}{"k": value}
	}
	_, err := Normalize(value, 0)
	if err == nil {
		t.Fatal("Normalize() error = nil, want depth limit rejection")
	}
}
