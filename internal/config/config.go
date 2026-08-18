package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/spf13/viper"
)

// Default pipeline quota limits (LWC-138).
const (
	DefaultPipelineDailyLimit                            = 2
	DefaultPipelineCooldownSeconds                       = 3600
	DefaultPipelineMinNewRaw                             = 1
	DefaultPipelineJobURL                                = "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run"
	DefaultAuthServiceURL                                = "https://auth.dev.rayer.idv.tw"
	DefaultQueryExpansionModel                           = "deepseek-v4-flash"
	DefaultAnswerSynthesisModel                          = "deepseek-v4-pro"
	DefaultQueryExpansionReasoning         llm.Reasoning = llm.ReasoningNone
	DefaultAnswerSynthesisReasoning        llm.Reasoning = llm.ReasoningNone
	DefaultQuerySelectionLimit                           = 10
	DefaultQuerySelectionExplorationSlots                = 1
	DefaultQuerySelectionEvidenceThreshold               = 1
	MaxQuerySelectionLimit                               = 1000
)

var defaultAllowedOrigins = []string{
	"https://wiki.rayer.idv.tw",
	"https://llm-wiki-frontend.vercel.app",
	"https://llm-wiki-bff-dev.rayer.idv.tw",
}

// Config holds application configuration loaded from config.toml.
type Config struct {
	GCPProject               string
	Bucket                   string
	FirestoreDatabaseID      string
	UserID                   string
	ProjectID                string
	Port                     string
	DeepSeekAPIKey           string
	QueryExpansionModel      string
	QueryExpansionReasoning  llm.Reasoning
	AnswerSynthesisModel     string
	AnswerSynthesisReasoning llm.Reasoning
	JWTSecret                string
	DevJWT                   bool
	LocalDataDir             string
	PipelineJobURL           string
	AllowedOrigins           []string
	AllowedHosts             []string
	Users                    []UserConfig

	// Pipeline quota (LWC-138). Env: PIPELINE_DAILY_LIMIT, PIPELINE_COOLDOWN_SECONDS,
	// PIPELINE_MIN_NEW_RAW, PIPELINE_DEMO_USER_IDS (comma-separated).
	PipelineDailyLimit      int
	PipelineCooldownSeconds int
	PipelineMinNewRaw       int
	PipelineDemoUserIDs     []string

	// Registration gate (LWC-149). Env: REGISTRATION_ENABLED (true/false/1/0).
	// Nil means unset; resolution falls back to default true when Firestore doc is absent.
	RegistrationEnabled *bool

	// AuthServiceURL is the public URL of the dedicated auth service (LWC-258).
	// Env: AUTH_SERVICE_URL. Default: https://auth.dev.rayer.idv.tw
	AuthServiceURL string

	// Query retrieval selection contract. Env: QUERY_SELECTION_LIMIT,
	// QUERY_SELECTION_EXPLORATION_SLOTS, QUERY_SELECTION_EVIDENCE_THRESHOLD.
	QuerySelectionLimit             int
	QuerySelectionExplorationSlots  int
	QuerySelectionEvidenceThreshold int
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
	v.SetDefault("query_expansion_model", DefaultQueryExpansionModel)
	v.SetDefault("query_expansion_reasoning", DefaultQueryExpansionReasoning)
	v.SetDefault("answer_synthesis_model", DefaultAnswerSynthesisModel)
	v.SetDefault("answer_synthesis_reasoning", DefaultAnswerSynthesisReasoning)
	v.SetDefault("query_selection_limit", DefaultQuerySelectionLimit)
	v.SetDefault("query_selection_exploration_slots", DefaultQuerySelectionExplorationSlots)
	v.SetDefault("query_selection_evidence_threshold", DefaultQuerySelectionEvidenceThreshold)
	v.AutomaticEnv()
	v.BindEnv("deepseek_api_key")
	v.BindEnv("firestore_database_id", "FIRESTORE_DATABASE_ID")
	v.BindEnv("pipeline_job_url", "PIPELINE_JOB_URL")
	v.BindEnv("allowed_origins", "ALLOWED_ORIGINS")
	v.BindEnv("allowed_hosts", "ALLOWED_HOSTS")
	v.BindEnv("pipeline_daily_limit", "PIPELINE_DAILY_LIMIT")
	v.BindEnv("pipeline_cooldown_seconds", "PIPELINE_COOLDOWN_SECONDS")
	v.BindEnv("pipeline_min_new_raw", "PIPELINE_MIN_NEW_RAW")
	v.BindEnv("pipeline_demo_user_ids", "PIPELINE_DEMO_USER_IDS")
	v.BindEnv("registration_enabled", "REGISTRATION_ENABLED")
	v.BindEnv("auth_service_url", "AUTH_SERVICE_URL")
	v.BindEnv("query_expansion_model", "QUERY_EXPANSION_MODEL")
	v.BindEnv("query_expansion_reasoning", "QUERY_EXPANSION_REASONING")
	v.BindEnv("answer_synthesis_model", "ANSWER_SYNTHESIS_MODEL")
	v.BindEnv("answer_synthesis_reasoning", "ANSWER_SYNTHESIS_REASONING")
	v.BindEnv("query_selection_limit", "QUERY_SELECTION_LIMIT")
	v.BindEnv("query_selection_exploration_slots", "QUERY_SELECTION_EXPLORATION_SLOTS")
	v.BindEnv("query_selection_evidence_threshold", "QUERY_SELECTION_EVIDENCE_THRESHOLD")

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
	if err := validatePipelineJobURL(pipelineJobURL); err != nil {
		return Config{}, fmt.Errorf("invalid pipeline_job_url: %w", err)
	}
	allowedHosts, err := parseAllowedHosts(v.GetString("allowed_hosts"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid allowed_hosts: %w", err)
	}

	var registrationEnabled *bool
	if raw := strings.TrimSpace(v.GetString("registration_enabled")); raw != "" {
		if enabled, ok := parseBoolEnv(raw); ok {
			registrationEnabled = &enabled
		}
	}

	authServiceURL := strings.TrimSpace(v.GetString("auth_service_url"))
	if authServiceURL == "" {
		authServiceURL = DefaultAuthServiceURL
	}
	queryExpansionModel := strings.TrimSpace(v.GetString("query_expansion_model"))
	if queryExpansionModel == "" {
		queryExpansionModel = DefaultQueryExpansionModel
	}
	if queryExpansionModel != DefaultQueryExpansionModel {
		return Config{}, fmt.Errorf("query_expansion_model must be %s", DefaultQueryExpansionModel)
	}
	queryExpansionReasoning := llm.Reasoning(strings.TrimSpace(v.GetString("query_expansion_reasoning")))
	answerSynthesisModel := strings.TrimSpace(v.GetString("answer_synthesis_model"))
	answerSynthesisReasoning := llm.Reasoning(strings.TrimSpace(v.GetString("answer_synthesis_reasoning")))
	if queryExpansionReasoning != DefaultQueryExpansionReasoning {
		return Config{}, fmt.Errorf("query_expansion_reasoning must be none")
	}
	if answerSynthesisModel != DefaultAnswerSynthesisModel {
		return Config{}, fmt.Errorf("answer_synthesis_model must be %s", DefaultAnswerSynthesisModel)
	}
	if !answerSynthesisReasoning.Valid() {
		return Config{}, fmt.Errorf("answer_synthesis_reasoning must be none, low, high, or max")
	}
	selectionLimit, err := configuredInt(v, "query_selection_limit", DefaultQuerySelectionLimit, 1, MaxQuerySelectionLimit)
	if err != nil {
		return Config{}, err
	}
	explorationSlots, err := configuredInt(v, "query_selection_exploration_slots", DefaultQuerySelectionExplorationSlots, 0, selectionLimit)
	if err != nil {
		return Config{}, err
	}
	evidenceThreshold, err := configuredInt(v, "query_selection_evidence_threshold", DefaultQuerySelectionEvidenceThreshold, 1, 0)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		GCPProject:                      v.GetString("gcp_project"),
		Bucket:                          v.GetString("bucket"),
		FirestoreDatabaseID:             strings.TrimSpace(v.GetString("firestore_database_id")),
		UserID:                          v.GetString("user_id"),
		ProjectID:                       v.GetString("project_id"),
		Port:                            v.GetString("port"),
		DeepSeekAPIKey:                  v.GetString("deepseek_api_key"),
		QueryExpansionModel:             queryExpansionModel,
		QueryExpansionReasoning:         queryExpansionReasoning,
		AnswerSynthesisModel:            answerSynthesisModel,
		AnswerSynthesisReasoning:        answerSynthesisReasoning,
		JWTSecret:                       v.GetString("jwt_secret"),
		DevJWT:                          v.GetBool("dev_jwt"),
		LocalDataDir:                    v.GetString("local_data_dir"),
		PipelineJobURL:                  pipelineJobURL,
		AllowedOrigins:                  parseAllowedOrigins(v.GetString("allowed_origins")),
		AllowedHosts:                    allowedHosts,
		PipelineDailyLimit:              dailyLimit,
		PipelineCooldownSeconds:         cooldownSeconds,
		PipelineMinNewRaw:               minNewRaw,
		PipelineDemoUserIDs:             splitCommaList(v.GetString("pipeline_demo_user_ids")),
		RegistrationEnabled:             registrationEnabled,
		AuthServiceURL:                  authServiceURL,
		QuerySelectionLimit:             selectionLimit,
		QuerySelectionExplorationSlots:  explorationSlots,
		QuerySelectionEvidenceThreshold: evidenceThreshold,
	}
	return cfg, nil
}

func configuredInt(v *viper.Viper, key string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(v.GetString(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: must be an integer", key)
	}
	if value < minimum {
		return 0, fmt.Errorf("invalid %s: must be at least %d", key, minimum)
	}
	if maximum > 0 && value > maximum {
		return 0, fmt.Errorf("invalid %s: must be at most %d", key, maximum)
	}
	return value, nil
}

func validatePipelineJobURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https")
	}
	if u.Host != "run.googleapis.com" {
		return fmt.Errorf("host must be run.googleapis.com")
	}
	if u.User != nil {
		return fmt.Errorf("userinfo is not allowed")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("query is not allowed")
	}
	if u.Fragment != "" {
		return fmt.Errorf("fragment is not allowed")
	}
	if u.RawPath != "" {
		return fmt.Errorf("escaped paths are not allowed")
	}

	parts := strings.Split(u.Path, "/")
	if len(parts) != 8 || parts[1] != "v2" || parts[2] != "projects" || parts[4] != "locations" || parts[6] != "jobs" {
		return fmt.Errorf("path must be /v2/projects/{project}/locations/{location}/jobs/{job}:run")
	}
	if !isSafePipelinePathSegment(parts[3]) || !isSafePipelinePathSegment(parts[5]) {
		return fmt.Errorf("project and location path segments must be non-empty and safe")
	}
	if !strings.HasSuffix(parts[7], ":run") || !isSafePipelinePathSegment(strings.TrimSuffix(parts[7], ":run")) {
		return fmt.Errorf("job path segment must end with :run and be safe")
	}
	return nil
}

func isSafePipelinePathSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	for _, r := range segment {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
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

// AllowedHostsFor returns configured hosts and adds loopback hosts in local mode.
func (c Config) AllowedHostsFor(localMode bool) []string {
	hosts := append([]string(nil), c.AllowedHosts...)
	if localMode {
		hosts = append(hosts, "localhost", "127.0.0.1")
	}
	return uniqueAllowedHosts(hosts)
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

func parseAllowedHosts(raw string) ([]string, error) {
	parts := splitCommaList(raw)
	for _, host := range parts {
		if strings.Contains(host, "*") {
			return nil, fmt.Errorf("wildcards are not allowed")
		}
	}
	return uniqueAllowedHosts(parts), nil
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

func uniqueAllowedHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	unique := make([]string, 0, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		unique = append(unique, host)
	}
	return unique
}
