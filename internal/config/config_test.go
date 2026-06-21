package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAllowsEmptyJWTSecretInProduction(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	dir := writeConfig(t, "dev_jwt = false\n")

	if _, err := Load(dir); err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
}

func TestLoadAllowsEmptyJWTSecretInDevelopment(t *testing.T) {
	dir := writeConfig(t, "dev_jwt = true\n")

	if _, err := Load(dir); err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}
