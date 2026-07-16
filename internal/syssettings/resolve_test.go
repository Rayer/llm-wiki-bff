package syssettings

import "testing"

func TestResolve_FirestoreWinsOverEnv(t *testing.T) {
	envFalse := false
	envTrue := true

	tests := []struct {
		name           string
		docExists      bool
		firestoreValue bool
		envValue       *bool
		want           bool
	}{
		{
			name:           "firestore false beats env true",
			docExists:      true,
			firestoreValue: false,
			envValue:       &envTrue,
			want:           false,
		},
		{
			name:           "firestore true beats env false",
			docExists:      true,
			firestoreValue: true,
			envValue:       &envFalse,
			want:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.docExists, tt.firestoreValue, tt.envValue)
			if got != tt.want {
				t.Fatalf("Resolve() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolve_EnvWhenNoFirestoreDoc(t *testing.T) {
	envFalse := false
	envTrue := true

	tests := []struct {
		name     string
		envValue *bool
		want     bool
	}{
		{name: "env true", envValue: &envTrue, want: true},
		{name: "env false", envValue: &envFalse, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(false, false, tt.envValue)
			if got != tt.want {
				t.Fatalf("Resolve() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolve_DefaultTrue(t *testing.T) {
	if got := Resolve(false, false, nil); got != true {
		t.Fatalf("Resolve() = %v, want true", got)
	}
}

func TestParseEnvBool(t *testing.T) {
	tests := []struct {
		raw    string
		want   bool
		wantOK bool
	}{
		{raw: "true", want: true, wantOK: true},
		{raw: "TRUE", want: true, wantOK: true},
		{raw: "1", want: true, wantOK: true},
		{raw: "false", want: false, wantOK: true},
		{raw: "0", want: false, wantOK: true},
		{raw: "", want: false, wantOK: false},
		{raw: "maybe", want: false, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := ParseEnvBool(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ParseEnvBool(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("ParseEnvBool(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}