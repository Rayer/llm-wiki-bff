package config

import (
	"errors"

	"github.com/spf13/viper"
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
	DefaultUserID  string
}

// Load reads config.toml from the given path and returns a Config.
func Load(path string) (Config, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("toml")
	v.AddConfigPath(path)
	v.SetDefault("port", "8080")
	v.AutomaticEnv()
	v.BindEnv("deepseek_api_key")

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return Config{}, err
		}
	}

	cfg := Config{
		GCPProject:     v.GetString("gcp_project"),
		Bucket:         v.GetString("bucket"),
		UserID:         v.GetString("user_id"),
		ProjectID:      v.GetString("project_id"),
		Port:           v.GetString("port"),
		DeepSeekAPIKey: v.GetString("deepseek_api_key"),
		JWTSecret:      v.GetString("jwt_secret"),
		DevJWT:         v.GetBool("dev_jwt"),
		DefaultUserID:  v.GetString("default_user_id"),
	}
	return cfg, nil
}
