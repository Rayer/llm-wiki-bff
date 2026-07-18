package generation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestDecodeBoundedStringListsNestedBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		counts  []int
		wantErr bool
	}{
		{name: "exact per list", counts: []int{MaxFiles, MaxFiles}},
		{name: "overflow one list", counts: []int{MaxFiles + 1, 1}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := json.NewDecoder(bytes.NewReader(stringListsJSON(tc.counts)))
			got, err := DecodeBoundedStringLists(dec)
			if tc.wantErr {
				if !errors.Is(err, ErrLogicalEntryLimit) {
					t.Fatalf("error = %v, want ErrLogicalEntryLimit", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || len(got["id-0"]) != MaxFiles || len(got["id-1"]) != MaxFiles {
				t.Fatalf("decoded list sizes = %d/%d, want %d/%d", len(got["id-0"]), len(got["id-1"]), MaxFiles, MaxFiles)
			}
		})
	}
}

func TestDecodeBoundedStringListsRejectsMalformedAndTrailingJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "value is not array", data: `{"id":"bad"}`},
		{name: "array value is not string", data: `{"id":[1]}`},
		{name: "malformed", data: `{"id":["a"]`},
		{name: "trailing", data: `{"id":["a"]} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := json.NewDecoder(bytes.NewBufferString(tc.data))
			_, err := DecodeBoundedStringLists(dec)
			if err == nil && tc.name != "trailing" {
				t.Fatal("expected decode error")
			}
			if tc.name == "trailing" {
				if err != nil {
					t.Fatalf("decode first value: %v", err)
				}
				if err := EnsureJSONEOF(dec); err == nil {
					t.Fatal("expected trailing JSON error")
				}
			}
		})
	}
}

func stringListsJSON(counts []int) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, count := range counts {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:[", fmt.Sprintf("id-%d", i))
		for j := 0; j < count; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"x"`)
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return b.Bytes()
}
