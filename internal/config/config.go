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
)

// Config holds application configuration loaded from config.toml.
type Config struct {
	GCPProject     string
	Bucket         string
	UserID         string
	ProjectID      string
	Port           string
	DeepSeekAPIKey string
	JWTSecret      string
	DevJWT         bool
	LocalDataDir   string
	Users          []UserConfig

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
	v.AutomaticEnv()
	v.BindEnv("deepseek_api_key")
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

	var registrationEnabled *bool
	if raw := strings.TrimSpace(v.GetString("registration_enabled")); raw != "" {
		if enabled, ok := parseBoolEnv(raw); ok {
			registrationEnabled = &enabled
		}
	}

	cfg := Config{
		GCPProject:              v.GetString("gcp_project"),
		Bucket:                  v.GetString("bucket"),
		UserID:                  v.GetString("user_id"),
		ProjectID:               v.GetString("project_id"),
		Port:                    v.GetString("port"),
		DeepSeekAPIKey:          v.GetString("deepseek_api_key"),
		JWTSecret:               v.GetString("jwt_secret"),
		DevJWT:                  v.GetBool("dev_jwt"),
		LocalDataDir:            v.GetString("local_data_dir"),
		PipelineDailyLimit:      dailyLimit,
		PipelineCooldownSeconds: cooldownSeconds,
		PipelineMinNewRaw:       minNewRaw,
		PipelineDemoUserIDs:     splitCommaList(v.GetString("pipeline_demo_user_ids")),
		RegistrationEnabled:     registrationEnabled,
	}
	return cfg, nil
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
