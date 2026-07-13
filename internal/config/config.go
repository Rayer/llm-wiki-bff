package config

import (
	"errors"
	"strings"

	"github.com/spf13/viper"
)

// Default pipeline quota limits (LWC-138).
const (
	DefaultPipelineDailyLimit      = 2
	DefaultPipelineCooldownSeconds = 3600
	DefaultPipelineMinNewRaw       = 1
	DefaultPipelineJobURL          = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
)

var defaultAllowedOrigins = []string{
	"https://wiki.rayer.idv.tw",
	"https://llm-wiki-frontend.vercel.app",
	"https://llm-wiki-bff-dev.rayer.idv.tw",
}

// Config holds application configuration loaded from config.toml.
type Config struct {
	GCPProject          string
	Bucket              string
	FirestoreDatabaseID string
	UserID              string
	ProjectID           string
	Port                string
	DeepSeekAPIKey      string
	JWTSecret           string
	DevJWT              bool
	LocalDataDir        string
	PipelineJobURL      string
	AllowedOrigins      []string
	Users               []UserConfig

	// Pipeline quota (LWC-138). Env: PIPELINE_DAILY_LIMIT, PIPELINE_COOLDOWN_SECONDS,
	// PIPELINE_MIN_NEW_RAW, PIPELINE_DEMO_USER_IDS (comma-separated).
	PipelineDailyLimit      int
	PipelineCooldownSeconds int
	PipelineMinNewRaw       int
	PipelineDemoUserIDs     []string

	// Registration gate (LWC-149). Env: REGISTRATION_ENABLED (true/false/1/0).
	// Nil means unset; resolution falls back to default true when Firestore doc is absent.
	RegistrationEnabled *bool
}

// UserConfig holds a hardcoded user for authentication.
type UserConfig struct {
	ID           string
	Email        string
	PasswordHash string
}

// Load reads config.toml from the given path and returns a Config.
func Load(path string) (Config, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("toml")
	v.AddConfigPath(path)
	v.SetDefault("port", "8080")
	v.SetDefault("pipeline_daily_limit", DefaultPipelineDailyLimit)
	v.SetDefault("pipeline_cooldown_seconds", DefaultPipelineCooldownSeconds)
	v.SetDefault("pipeline_min_new_raw", DefaultPipelineMinNewRaw)
	v.SetDefault("pipeline_job_url", DefaultPipelineJobURL)
	v.AutomaticEnv()
	v.BindEnv("deepseek_api_key")
	v.BindEnv("firestore_database_id", "FIRESTORE_DATABASE_ID")
	v.BindEnv("pipeline_job_url", "PIPELINE_JOB_URL")
	v.BindEnv("allowed_origins", "ALLOWED_ORIGINS")
	v.BindEnv("pipeline_daily_limit", "PIPELINE_DAILY_LIMIT")
	v.BindEnv("pipeline_cooldown_seconds", "PIPELINE_COOLDOWN_SECONDS")
	v.BindEnv("pipeline_min_new_raw", "PIPELINE_MIN_NEW_RAW")
	v.BindEnv("pipeline_demo_user_ids", "PIPELINE_DEMO_USER_IDS")
	v.BindEnv("registration_enabled", "REGISTRATION_ENABLED")

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return Config{}, err
		}
	}

	dailyLimit := v.GetInt("pipeline_daily_limit")
	if dailyLimit <= 0 {
		dailyLimit = DefaultPipelineDailyLimit
	}
	cooldownSeconds := v.GetInt("pipeline_cooldown_seconds")
	if cooldownSeconds <= 0 {
		cooldownSeconds = DefaultPipelineCooldownSeconds
	}
	minNewRaw := v.GetInt("pipeline_min_new_raw")
	if minNewRaw <= 0 {
		minNewRaw = DefaultPipelineMinNewRaw
	}
	pipelineJobURL := strings.TrimSpace(v.GetString("pipeline_job_url"))
	if pipelineJobURL == "" {
		pipelineJobURL = DefaultPipelineJobURL
	}

	var registrationEnabled *bool
	if raw := strings.TrimSpace(v.GetString("registration_enabled")); raw != "" {
		if enabled, ok := parseBoolEnv(raw); ok {
			registrationEnabled = &enabled
		}
	}

	cfg := Config{
		GCPProject:              v.GetString("gcp_project"),
		Bucket:                  v.GetString("bucket"),
		FirestoreDatabaseID:     strings.TrimSpace(v.GetString("firestore_database_id")),
		UserID:                  v.GetString("user_id"),
		ProjectID:               v.GetString("project_id"),
		Port:                    v.GetString("port"),
		DeepSeekAPIKey:          v.GetString("deepseek_api_key"),
		JWTSecret:               v.GetString("jwt_secret"),
		DevJWT:                  v.GetBool("dev_jwt"),
		LocalDataDir:            v.GetString("local_data_dir"),
		PipelineJobURL:          pipelineJobURL,
		AllowedOrigins:          parseAllowedOrigins(v.GetString("allowed_origins")),
		PipelineDailyLimit:      dailyLimit,
		PipelineCooldownSeconds: cooldownSeconds,
		PipelineMinNewRaw:       minNewRaw,
		PipelineDemoUserIDs:     splitCommaList(v.GetString("pipeline_demo_user_ids")),
		RegistrationEnabled:     registrationEnabled,
	}
	return cfg, nil
}

// AllowedOriginsFor returns configured origins and adds local development
// origins when the BFF is running in local mode.
func (c Config) AllowedOriginsFor(localMode bool) []string {
	origins := append([]string(nil), c.AllowedOrigins...)
	if localMode {
		origins = append(origins, "http://localhost:3000", "http://127.0.0.1:3000")
	}
	return uniqueAllowedOrigins(origins)
}

func parseBoolEnv(raw string) (bool, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch raw {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

func splitCommaList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseAllowedOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), defaultAllowedOrigins...)
	}

	parts := strings.Split(raw, ",")
	origins := make([]string, 0, len(parts))
	for _, part := range parts {
		origin := strings.TrimSpace(part)
		if origin == "" || origin == "*" {
			continue
		}
		origins = append(origins, origin)
	}
	return uniqueAllowedOrigins(origins)
}

func uniqueAllowedOrigins(origins []string) []string {
	seen := make(map[string]struct{}, len(origins))
	unique := make([]string, 0, len(origins))
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin == "" || origin == "*" {
			continue
		}
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		unique = append(unique, origin)
	}
	return unique
}
