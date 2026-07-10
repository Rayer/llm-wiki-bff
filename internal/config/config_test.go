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

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}
