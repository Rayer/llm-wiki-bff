package main

import (
	"errors"

	"github.com/spf13/viper"
)

type config struct {
	GCPProject     string
	Bucket         string
	UserID         string
	ProjectID      string
	Port           string
	DeepSeekAPIKey string
}

func loadConfig(path string) (config, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("toml")
	v.AddConfigPath(path)
	v.SetDefault("port", "8080")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return config{}, err
		}
	}

	return config{
		GCPProject:     v.GetString("gcp_project"),
		Bucket:         v.GetString("bucket"),
		UserID:         v.GetString("user_id"),
		ProjectID:      v.GetString("project_id"),
		Port:           v.GetString("port"),
		DeepSeekAPIKey: v.GetString("deepseek_api_key"),
	}, nil
}
