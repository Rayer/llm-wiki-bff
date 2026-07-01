package config

import (
	"errors"

	"github.com/spf13/viper"
)

// Config holds application configuration loaded from config.toml.
type Config struct {
	GCPProject     string
	Bucket         string
	Port           string
	DeepSeekAPIKey string
	JWTSecret      string
	DevJWT         bool
	Users          []UserConfig
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
		Port:           v.GetString("port"),
		DeepSeekAPIKey: v.GetString("deepseek_api_key"),
		JWTSecret:      v.GetString("jwt_secret"),
		DevJWT:         v.GetBool("dev_jwt"),
	}
	return cfg, nil
}
