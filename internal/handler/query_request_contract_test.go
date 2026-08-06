package handler

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestQueryRequestJSONContract(t *testing.T) {
	t.Run("exact public json fields", func(t *testing.T) {
		typeRef := reflect.TypeOf(QueryRequest{})
		actual := collectJSONFieldNames(typeRef)

		if hasProject := actual["project"]; hasProject {
			t.Fatalf("handler.QueryRequest still binds JSON field \"project\": %#v", actual)
		}

		got := keysSorted(actual)
		want := []string{"mode", "q"}
		if len(got) != len(want) {
			t.Fatalf("handler.QueryRequest JSON field set = %q, want %q", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("handler.QueryRequest JSON field set = %q, want %q", got, want)
			}
		}
	})
}

func collectJSONFieldNames(typeRef reflect.Type) map[string]bool {
	fields := make(map[string]bool)
	for i := 0; i < typeRef.NumField(); i++ {
		field := typeRef.Field(i)
		jsonField, ok := jsonFieldName(field)
		if !ok {
			continue
		}
		fields[jsonField] = true
	}
	return fields
}

func jsonFieldName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	if tag == "" {
		if field.PkgPath != "" {
			return "", false
		}
		return field.Name, true
	}
	name := strings.Split(tag, ",")[0]
	if name == "-" {
		return "", false
	}
	if name == "" {
		if field.PkgPath != "" {
			return "", false
		}
		return field.Name, true
	}
	return name, true
}

func keysSorted(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestQueryRequestCanMarshalModeAndQuery(t *testing.T) {
	payload := QueryRequest{Query: "coffee", Mode: "wiki"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["q"]; !ok {
		t.Fatalf("query payload must include q: %s", string(encoded))
	}
	if _, ok := decoded["mode"]; !ok {
		t.Fatalf("query payload must include mode: %s", string(encoded))
	}
	if _, ok := decoded["project"]; ok {
		t.Fatalf("query payload must not include project: %s", string(encoded))
	}
}
