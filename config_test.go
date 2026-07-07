package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadsTOMLFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(`
gcp_project = "wiki-gcp"
bucket = "wiki-bucket"
port = "9090"
dev_jwt = true
`), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.GCPProject != "wiki-gcp" {
		t.Fatalf("GCPProject = %q, want %q", cfg.GCPProject, "wiki-gcp")
	}
	if cfg.Bucket != "wiki-bucket" {
		t.Fatalf("Bucket = %q, want %q", cfg.Bucket, "wiki-bucket")
	}
	if cfg.Port != "9090" {
		t.Fatalf("Port = %q, want %q", cfg.Port, "9090")
	}
}

func TestLoadConfigDefaultsPortAndAllowsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(`
gcp_project = "wiki-gcp"
bucket = "wiki-bucket"
dev_jwt = true
`), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	t.Setenv("PORT", "7070")

	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Port != "7070" {
		t.Fatalf("Port = %q, want env override %q", cfg.Port, "7070")
	}
}
