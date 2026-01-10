package config

import (
	"bufio"
	"os"
	"strings"
)

type Config struct {
	Port    string
	AppName string
	Version string
}

func Load(appName, defaultVersion string) *Config {
	// 1. Attempt to load version from service.yaml (Source of Truth)
	serviceVersion, serviceName := readServiceMetadata()

	finalVersion := defaultVersion
	if serviceVersion != "" {
		finalVersion = serviceVersion
	}

	finalAppName := appName
	if finalAppName == "" && serviceName != "" {
		finalAppName = serviceName
	}

	// 2. Load Port (Env > Default)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	return &Config{
		Port:    port,
		AppName: finalAppName,
		Version: finalVersion,
	}
}

// readServiceMetadata attempts to parse 'service.yaml' in the current directory
// to extract 'version' and 'name'. It uses a simple scanner to avoid external YAML deps.
func readServiceMetadata() (version, name string) {
	f, err := os.Open("service.yaml")
	if err != nil {
		return "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "version:") {
			version = strings.TrimSpace(strings.TrimPrefix(line, "version:"))
			version = strings.Trim(version, `"'`) // Remove quotes
		}
		if strings.HasPrefix(line, "name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
			name = strings.Trim(name, `"'`)
		}
	}
	return version, name
}
