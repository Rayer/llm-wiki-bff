package llm

import (
	"testing"
)

func TestParseExpandResult(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    *ExpandResult
		wantNil bool
	}{
		{
			name: "valid JSON with additional_request",
			raw:  `{"keywords":["親子","公園"],"districts":["大安區"],"facilities":["遊樂場"],"suggestions":["早上去"],"additional_request":["雨天備案","寵物友善"]}`,
			want: &ExpandResult{
				Keywords:          []string{"親子", "公園"},
				Districts:         []string{"大安區"},
				Facilities:        []string{"遊樂場"},
				Suggestions:       []string{"早上去"},
				AdditionalRequest: []string{"雨天備案", "寵物友善"},
			},
		},
		{
			name: "valid JSON without additional_request",
			raw:  `{"keywords":["親子","公園"],"districts":["大安區"],"facilities":["遊樂場"],"suggestions":["早上去"]}`,
			want: &ExpandResult{
				Keywords:    []string{"親子", "公園"},
				Districts:   []string{"大安區"},
				Facilities:  []string{"遊樂場"},
				Suggestions: []string{"早上去"},
			},
		},
		{
			name: "markdown code fence wrapped",
			raw:  "```json\n{\"keywords\":[\"a\",\"b\"],\"suggestions\":[\"tip\"]}\n```",
			want: &ExpandResult{
				Keywords:    []string{"a", "b"},
				Suggestions: []string{"tip"},
			},
		},
		{
			name:    "empty keywords",
			raw:     `{"keywords":[],"suggestions":["tip"]}`,
			wantNil: true,
		},
		{
			name:    "not JSON",
			raw:     "台北 親子 公園",
			wantNil: true,
		},
		{
			name:    "empty string",
			raw:     "",
			wantNil: true,
		},
		{
			name:    "only whitespace",
			raw:     "   ",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := parseExpandResult(tt.raw)
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %+v", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			if len(result.Keywords) != len(tt.want.Keywords) {
				t.Errorf("keywords: got %v, want %v", result.Keywords, tt.want.Keywords)
			}
			if len(result.Districts) != len(tt.want.Districts) {
				t.Errorf("districts: got %v, want %v", result.Districts, tt.want.Districts)
			}
			if len(result.Suggestions) != len(tt.want.Suggestions) {
				t.Errorf("suggestions: got %v, want %v", result.Suggestions, tt.want.Suggestions)
			}
		})
	}
}

func TestLoadPrompt(t *testing.T) {
	// Verify embedded prompts load
	prompt, err := loadPrompt("lifestyle")
	if err != nil {
		t.Fatalf("load lifestyle: %v", err)
	}
	if prompt == "" {
		t.Fatal("lifestyle prompt is empty")
	}
	if !contains(prompt, "台灣在地生活專家") {
		t.Error("lifestyle prompt missing expected content")
	}

	prompt, err = loadPrompt("default")
	if err != nil {
		t.Fatalf("load default: %v", err)
	}
	if prompt == "" {
		t.Fatal("default prompt is empty")
	}

	prompt, err = loadPrompt("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent domain")
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
