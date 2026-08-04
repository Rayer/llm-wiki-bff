package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerBuildNonceValidatorScript(t *testing.T) {
	script := filepath.Join("..", "..", "cmd", "olw_worker", "validate_build_nonce.sh")

	type tc struct {
		name    string
		value   string
		wantErr bool
	}

	for _, c := range []tc{
		{
			name:    "valid_32_lowercase_hex",
			value:   "0123456789abcdef0123456789abcdef",
			wantErr: false,
		},
		{
			name:    "empty",
			value:   "",
			wantErr: true,
		},
		{
			name:    "31_chars",
			value:   "0123456789abcdef0123456789abcde",
			wantErr: true,
		},
		{
			name:    "33_chars",
			value:   "0123456789abcdef0123456789abcdef0",
			wantErr: true,
		},
		{
			name:    "uppercase",
			value:   "0123456789ABCDEF0123456789ABCDEF",
			wantErr: true,
		},
		{
			name:    "invalid_character_g",
			value:   "0123456789abcdef0123456789abcdeg",
			wantErr: true,
		},
		{
			name:    "spaces_and_newline",
			value:   "0123456789abcdef0123456789abcdef\n ",
			wantErr: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("sh", script, c.value)
			out, err := cmd.CombinedOutput()
			if c.wantErr {
				if err == nil {
					t.Fatalf("validator accepted %q", c.value)
				}
				output := string(out)
				if c.value != "" && strings.Contains(output, c.value) {
					t.Fatalf("validator leaked nonce-like output %q", c.value)
				}
				if strings.Contains(output, "build_nonce=") {
					t.Fatalf("validator exposed nonce content in failure output: %q", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("validator rejected valid input %q: %v output=%q", c.value, err, out)
			}
		})
	}
}
