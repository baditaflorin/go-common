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
//
// Only top-level keys count: an indented "name:" belongs to a nested block such
// as owner:, and must not replace the service's own name. A missing file is
// normal (tests, `go run` outside the service directory) and is silent.
func readServiceMetadata() (version, name string) {
	file, err := os.Open("service.yaml")
	if err != nil {
		return "", ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "version:"); ok {
			version = yamlScalar(rest)
		}
		if rest, ok := strings.CutPrefix(line, "name:"); ok {
			name = yamlScalar(rest)
		}
	}
	return version, name
}

// yamlScalar returns the value of a simple "key: value" line: unquoted, with any
// trailing comment removed.
func yamlScalar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if q := s[0]; q == '"' || q == '\'' {
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			return s[1 : 1+end]
		}
		return strings.Trim(s, `"'`) // unterminated quote: keep the old behaviour
	}
	if strings.HasPrefix(s, "#") {
		return ""
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
