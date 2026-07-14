package config

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestLoadPipelineQuotaDefaults(t *testing.T) {
	// Clear env so defaults apply (t.Setenv restores after test).
	t.Setenv("PIPELINE_DAILY_LIMIT", "")
	t.Setenv("PIPELINE_COOLDOWN_SECONDS", "")
	t.Setenv("PIPELINE_MIN_NEW_RAW", "")
	t.Setenv("PIPELINE_DEMO_USER_IDS", "")

	dir := writeConfig(t, "dev_jwt = true\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PipelineDailyLimit != DefaultPipelineDailyLimit {
		t.Fatalf("PipelineDailyLimit = %d, want %d", cfg.PipelineDailyLimit, DefaultPipelineDailyLimit)
	}
	if cfg.PipelineCooldownSeconds != DefaultPipelineCooldownSeconds {
		t.Fatalf("PipelineCooldownSeconds = %d, want %d", cfg.PipelineCooldownSeconds, DefaultPipelineCooldownSeconds)
	}
	if cfg.PipelineMinNewRaw != DefaultPipelineMinNewRaw {
		t.Fatalf("PipelineMinNewRaw = %d, want %d", cfg.PipelineMinNewRaw, DefaultPipelineMinNewRaw)
	}
	if len(cfg.PipelineDemoUserIDs) != 0 {
		t.Fatalf("PipelineDemoUserIDs = %#v, want empty", cfg.PipelineDemoUserIDs)
	}
}

func TestLoadPipelineQuotaFromEnv(t *testing.T) {
	t.Setenv("PIPELINE_DAILY_LIMIT", "5")
	t.Setenv("PIPELINE_COOLDOWN_SECONDS", "120")
	t.Setenv("PIPELINE_MIN_NEW_RAW", "3")
	t.Setenv("PIPELINE_DEMO_USER_IDS", " demo-user , other-demo , ")

	dir := writeConfig(t, "dev_jwt = true\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PipelineDailyLimit != 5 {
		t.Fatalf("PipelineDailyLimit = %d, want 5", cfg.PipelineDailyLimit)
	}
	if cfg.PipelineCooldownSeconds != 120 {
		t.Fatalf("PipelineCooldownSeconds = %d, want 120", cfg.PipelineCooldownSeconds)
	}
	if cfg.PipelineMinNewRaw != 3 {
		t.Fatalf("PipelineMinNewRaw = %d, want 3", cfg.PipelineMinNewRaw)
	}
	wantIDs := []string{"demo-user", "other-demo"}
	if !reflect.DeepEqual(cfg.PipelineDemoUserIDs, wantIDs) {
		t.Fatalf("PipelineDemoUserIDs = %#v, want %#v", cfg.PipelineDemoUserIDs, wantIDs)
	}
}

func TestLoadPipelineQuotaZeroEnvUsesDefaults(t *testing.T) {
	t.Setenv("PIPELINE_DAILY_LIMIT", "0")
	t.Setenv("PIPELINE_COOLDOWN_SECONDS", "0")
	t.Setenv("PIPELINE_MIN_NEW_RAW", "0")

	dir := writeConfig(t, "dev_jwt = true\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PipelineDailyLimit != DefaultPipelineDailyLimit {
		t.Fatalf("PipelineDailyLimit = %d, want default %d", cfg.PipelineDailyLimit, DefaultPipelineDailyLimit)
	}
	if cfg.PipelineCooldownSeconds != DefaultPipelineCooldownSeconds {
		t.Fatalf("PipelineCooldownSeconds = %d, want default %d", cfg.PipelineCooldownSeconds, DefaultPipelineCooldownSeconds)
	}
	if cfg.PipelineMinNewRaw != DefaultPipelineMinNewRaw {
		t.Fatalf("PipelineMinNewRaw = %d, want default %d", cfg.PipelineMinNewRaw, DefaultPipelineMinNewRaw)
	}
}

func TestLoadRegistrationEnabledFromEnv(t *testing.T) {
	t.Setenv("REGISTRATION_ENABLED", "false")

	dir := writeConfig(t, "dev_jwt = true\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.RegistrationEnabled == nil {
		t.Fatal("RegistrationEnabled = nil, want pointer to false")
	}
	if *cfg.RegistrationEnabled {
		t.Fatalf("RegistrationEnabled = true, want false")
	}
}

func TestLoadRegistrationEnabledUnset(t *testing.T) {
	t.Setenv("REGISTRATION_ENABLED", "")

	dir := writeConfig(t, "dev_jwt = true\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.RegistrationEnabled != nil {
		t.Fatalf("RegistrationEnabled = %#v, want nil when env unset", cfg.RegistrationEnabled)
	}
}

func TestLoadEnvironmentSelectionDefaults(t *testing.T) {
	t.Setenv("FIRESTORE_DATABASE_ID", "")
	t.Setenv("PIPELINE_JOB_URL", "")
	t.Setenv("ALLOWED_ORIGINS", "")

	cfg, err := Load(writeConfig(t, "dev_jwt = true\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FirestoreDatabaseID != "" {
		t.Fatalf("FirestoreDatabaseID = %q, want empty default", cfg.FirestoreDatabaseID)
	}
	if cfg.PipelineJobURL != DefaultPipelineJobURL {
		t.Fatalf("PipelineJobURL = %q, want %q", cfg.PipelineJobURL, DefaultPipelineJobURL)
	}
	wantOrigins := []string{
		"https://wiki.rayer.idv.tw",
		"https://llm-wiki-frontend.vercel.app",
		"https://llm-wiki-bff-dev.rayer.idv.tw",
	}
	if !reflect.DeepEqual(cfg.AllowedOrigins, wantOrigins) {
		t.Fatalf("AllowedOrigins = %#v, want %#v", cfg.AllowedOrigins, wantOrigins)
	}
}

func TestLoadEnvironmentSelectionFromEnv(t *testing.T) {
	t.Setenv("FIRESTORE_DATABASE_ID", " llm-wiki-cloud-dev ")
	t.Setenv("PIPELINE_JOB_URL", " https://run.googleapis.com/v2/projects/p/locations/r/jobs/olw-pipeline-dev:run ")
	t.Setenv("ALLOWED_ORIGINS", " https://dev.example, https://dev.example, *, http://localhost:3000 ")

	cfg, err := Load(writeConfig(t, "dev_jwt = true\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.FirestoreDatabaseID != "llm-wiki-cloud-dev" {
		t.Fatalf("FirestoreDatabaseID = %q", cfg.FirestoreDatabaseID)
	}
	if cfg.PipelineJobURL != "https://run.googleapis.com/v2/projects/p/locations/r/jobs/olw-pipeline-dev:run" {
		t.Fatalf("PipelineJobURL = %q", cfg.PipelineJobURL)
	}
	wantOrigins := []string{"https://dev.example", "http://localhost:3000"}
	if !reflect.DeepEqual(cfg.AllowedOrigins, wantOrigins) {
		t.Fatalf("AllowedOrigins = %#v, want %#v", cfg.AllowedOrigins, wantOrigins)
	}
	if got := cfg.AllowedOriginsFor(true); !reflect.DeepEqual(got, append(wantOrigins, "http://127.0.0.1:3000")) {
		t.Fatalf("AllowedOriginsFor(local) = %#v", got)
	}
}

func TestLoadRejectsInvalidPipelineJobURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "http", url: "http://run.googleapis.com/v2/projects/p/locations/r/jobs/j:run"},
		{name: "malicious host", url: "https://attacker.example/v2/projects/p/locations/r/jobs/j:run"},
		{name: "userinfo", url: "https://attacker.example@run.googleapis.com/v2/projects/p/locations/r/jobs/j:run"},
		{name: "query", url: "https://run.googleapis.com/v2/projects/p/locations/r/jobs/j:run?token=leak"},
		{name: "fragment", url: "https://run.googleapis.com/v2/projects/p/locations/r/jobs/j:run#fragment"},
		{name: "malformed suffix", url: "https://run.googleapis.com/v2/projects/p/locations/r/jobs/j:invoke"},
		{name: "empty location segment", url: "https://run.googleapis.com/v2/projects/p/locations//jobs/j:run"},
		{name: "unsafe project segment", url: "https://run.googleapis.com/v2/projects/p%2Fattacker/locations/r/jobs/j:run"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PIPELINE_JOB_URL", tt.url)
			if _, err := Load(writeConfig(t, "dev_jwt = true\n")); err == nil {
				t.Fatalf("Load() accepted invalid pipeline job URL %q", tt.url)
			}
		})
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
