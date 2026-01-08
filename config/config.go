package config

import (
	"os"
)

type Config struct {
	Port    string
	AppName string
	Version string
}

func Load(appName, version string) *Config {
	// We assume godotenv is loaded by main or we load it here?
	// Keep it simple: Assume Env vars are set or .env is present.
	// But library shouldn't maybe force godotenv?
	// We'll enforce Standard Port logic here.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Default fallback
	}
	return &Config{
		Port:    port,
		AppName: appName,
		Version: version,
	}
}
