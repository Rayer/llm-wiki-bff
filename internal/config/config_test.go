package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadStageConfigPathTracksAuthorityAndRejectsMixedLegacyFactors(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "/app/configs/query/dev/query-dev-2026-08-22.1.json")
	for _, name := range []string{
		"QUERY_EXPANSION_MODEL", "QUERY_EXPANSION_REASONING", "ANSWER_SYNTHESIS_MODEL", "ANSWER_SYNTHESIS_REASONING",
		"QUERY_SELECTION_LIMIT", "QUERY_SELECTION_EXPLORATION_SLOTS", "QUERY_SELECTION_EVIDENCE_THRESHOLD",
		"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT", "QUERY_EXPANSION_ATTEMPTS", "QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY",
	} {
		t.Setenv(name, "")
	}
	cfg, err := Load(writeConfig(t, "dev_jwt = true\n"))
	if err != nil {
		t.Fatalf("defaults unexpectedly became explicit: %v", err)
	}
	if cfg.QueryStageConfigPath == "" {
		t.Fatal("stage config path was not loaded")
	}
	t.Setenv("QUERY_SELECTION_LIMIT", "9")
	if _, err := Load(writeConfig(t, "dev_jwt = true\n")); !errors.Is(err, ErrMixedQueryConfigAuthority) {
		t.Fatalf("mixed authority error=%v, want %v", err, ErrMixedQueryConfigAuthority)
	}
	if _, err := Load(writeConfig(t, "dev_jwt = true\nquery_selection_limit = 9\n")); !errors.Is(err, ErrMixedQueryConfigAuthority) {
		t.Fatalf("TOML mixed authority error=%v, want %v", err, ErrMixedQueryConfigAuthority)
	}
}

func TestLoadDefaultsQueryExpansionModel(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	t.Setenv("QUERY_EXPANSION_MODEL", "")
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueryExpansionModel != "deepseek-v4-flash" || cfg.QueryExpansionReasoning != "none" {
		t.Fatalf("expansion config = %#v, want flash/none", cfg)
	}
	if cfg.AnswerSynthesisModel != "deepseek-v4-pro" || cfg.AnswerSynthesisReasoning != "none" {
		t.Fatalf("synthesis config = %#v, want pro/none", cfg)
	}
}

func TestLoadDefaultsAndEnvForParallelQueryExpansion(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	for _, name := range []string{
		"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT",
		"QUERY_EXPANSION_ATTEMPTS",
		"QUERY_SELECTION_EVIDENCE_THRESHOLD",
		"QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY",
	} {
		t.Setenv(name, "")
	}
	cfg, err := Load(writeConfig(t, "dev_jwt = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueryExpansionKeywordsPerAttempt != 24 || cfg.QueryExpansionAttempts != 3 || cfg.QuerySelectionEvidenceThreshold != 2 || cfg.QueryMatchingRareKeywordMaxDocumentFrequency != 1 {
		t.Fatalf("parallel expansion defaults = %#v", cfg)
	}

	t.Setenv("QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT", "12")
	t.Setenv("QUERY_EXPANSION_ATTEMPTS", "2")
	t.Setenv("QUERY_SELECTION_EVIDENCE_THRESHOLD", "3")
	t.Setenv("QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY", "4")
	cfg, err = Load(writeConfig(t, "dev_jwt = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueryExpansionKeywordsPerAttempt != 12 || cfg.QueryExpansionAttempts != 2 || cfg.QuerySelectionEvidenceThreshold != 3 || cfg.QueryMatchingRareKeywordMaxDocumentFrequency != 4 {
		t.Fatalf("parallel expansion env = %#v", cfg)
	}
}

func TestLoadRejectsUnsafeParallelQueryExpansionConfig(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	for _, test := range []struct {
		name string
		env  map[string]string
	}{
		{name: "keywords zero", env: map[string]string{"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT": "0"}},
		{name: "attempts negative", env: map[string]string{"QUERY_EXPANSION_ATTEMPTS": "-1"}},
		{name: "attempts excessive", env: map[string]string{"QUERY_EXPANSION_ATTEMPTS": "11"}},
		{name: "keywords malformed", env: map[string]string{"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT": "nope"}},
		{name: "rare frequency negative", env: map[string]string{"QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY": "-1"}},
		{name: "threshold malformed", env: map[string]string{"QUERY_SELECTION_EVIDENCE_THRESHOLD": "nope"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, name := range []string{"QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT", "QUERY_EXPANSION_ATTEMPTS", "QUERY_SELECTION_EVIDENCE_THRESHOLD", "QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY"} {
				t.Setenv(name, "")
			}
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			if _, err := Load(writeConfig(t, "dev_jwt = true\n")); err == nil {
				t.Fatal("Load() accepted unsafe parallel expansion configuration")
			}
		})
	}
}

func TestLoadRejectsUnknownQueryConfiguration(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	if _, err := Load(writeConfig(t, "dev_jwt = true\nquery_expansion_attempts_typo = 3\n")); err == nil {
		t.Fatal("Load() accepted unknown query configuration")
	}
}

func TestLoadQuerySelectionDefaultsAndTypedEnv(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	t.Setenv("QUERY_SELECTION_LIMIT", "")
	t.Setenv("QUERY_SELECTION_EXPLORATION_SLOTS", "")
	t.Setenv("QUERY_SELECTION_EVIDENCE_THRESHOLD", "")
	cfg, err := Load(writeConfig(t, "dev_jwt = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuerySelectionLimit != DefaultQuerySelectionLimit || cfg.QuerySelectionExplorationSlots != DefaultQuerySelectionExplorationSlots || cfg.QuerySelectionEvidenceThreshold != DefaultQuerySelectionEvidenceThreshold {
		t.Fatalf("query selection defaults = %#v", cfg)
	}
	t.Setenv("QUERY_SELECTION_LIMIT", "7")
	t.Setenv("QUERY_SELECTION_EXPLORATION_SLOTS", "2")
	t.Setenv("QUERY_SELECTION_EVIDENCE_THRESHOLD", "3")
	cfg, err = Load(writeConfig(t, "dev_jwt = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QuerySelectionLimit != 7 || cfg.QuerySelectionExplorationSlots != 2 || cfg.QuerySelectionEvidenceThreshold != 3 {
		t.Fatalf("query selection env = %#v", cfg)
	}
}

func TestLoadRejectsMalformedOrUnsafeQuerySelectionConfig(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	tests := []struct {
		name string
		env  string
	}{
		{name: "threshold zero", env: "QUERY_SELECTION_EVIDENCE_THRESHOLD=0"},
		{name: "threshold malformed", env: "QUERY_SELECTION_EVIDENCE_THRESHOLD=nope"},
		{name: "limit zero", env: "QUERY_SELECTION_LIMIT=0"},
		{name: "limit too large", env: "QUERY_SELECTION_LIMIT=1001"},
		{name: "exploration negative", env: "QUERY_SELECTION_EXPLORATION_SLOTS=-1"},
		{name: "exploration over limit", env: "QUERY_SELECTION_LIMIT=2\nQUERY_SELECTION_EXPLORATION_SLOTS=3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("QUERY_SELECTION_LIMIT", "")
			t.Setenv("QUERY_SELECTION_EXPLORATION_SLOTS", "")
			t.Setenv("QUERY_SELECTION_EVIDENCE_THRESHOLD", "")
			for _, line := range strings.Split(test.env, "\n") {
				parts := strings.SplitN(line, "=", 2)
				t.Setenv(parts[0], parts[1])
			}
			if _, err := Load(writeConfig(t, "dev_jwt = true\n")); err == nil {
				t.Fatal("Load() accepted invalid query selection configuration")
			}
		})
	}
}

func TestLoadRejectsNonBaselineQueryExpansionModel(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	t.Setenv("QUERY_EXPANSION_MODEL", "deepseek-chat")
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load() error = nil, want fixed baseline rejection")
	}
}

func TestLoadRejectsEnabledExpansionAndInvalidSynthesisReasoning(t *testing.T) {
	t.Setenv("QUERY_STAGE_CONFIG_PATH", "")
	t.Setenv("QUERY_EXPANSION_REASONING", "low")
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load() accepted enabled expansion reasoning")
	}
	t.Setenv("QUERY_EXPANSION_REASONING", "none")
	t.Setenv("ANSWER_SYNTHESIS_REASONING", "medium")
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load() accepted invalid synthesis reasoning")
	}
}

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
	t.Setenv("ALLOWED_HOSTS", "")

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

func TestLoadAllowedHostsFromEnv(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", " auth.dev.rayer.idv.tw, auth-dev.rayer.idv.tw ")

	cfg, err := Load(writeConfig(t, "dev_jwt = false\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"auth.dev.rayer.idv.tw", "auth-dev.rayer.idv.tw"}
	if !reflect.DeepEqual(cfg.AllowedHosts, want) {
		t.Fatalf("AllowedHosts = %#v, want %#v", cfg.AllowedHosts, want)
	}
}

func TestLoadDevMigrationOriginsFromEnv(t *testing.T) {
	t.Setenv("ALLOWED_ORIGINS", "https://wiki.dev.rayer.idv.tw, https://llm-wiki-frontend-dev.vercel.app")

	cfg, err := Load(writeConfig(t, "dev_jwt = false\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"https://wiki.dev.rayer.idv.tw", "https://llm-wiki-frontend-dev.vercel.app"}
	if !reflect.DeepEqual(cfg.AllowedOrigins, want) {
		t.Fatalf("AllowedOrigins = %#v, want %#v", cfg.AllowedOrigins, want)
	}
}

func TestLoadRejectsWildcardAllowedHost(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", "*")

	if _, err := Load(writeConfig(t, "dev_jwt = false\n")); err == nil {
		t.Fatal("Load() accepted wildcard ALLOWED_HOSTS")
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
