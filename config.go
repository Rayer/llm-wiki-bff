package main

import "github.com/rayert/llm-wiki-bff/internal/config"

// loadConfig reads config.toml from dir and applies env overrides.
func loadConfig(dir string) (config.Config, error) {
	return config.Load(dir)
}
