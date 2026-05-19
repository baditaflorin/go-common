// Package config loads service configuration from service.yaml and
// environment variables.
//
// Usage:
//
//	cfg := config.Load("go_myservice", "v1.0.0")
//	// cfg.Port, cfg.AppName, cfg.Version
//
// For build-identity (git tag, commit SHA) see the version package.
package config
