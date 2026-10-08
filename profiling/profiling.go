// Package profiling provides opt-in continuous Go profiling through Grafana
// Pyroscope. Importing this package has no runtime effect until a server
// address is configured.
package profiling

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	pyroscope "github.com/grafana/pyroscope-go"
)

const (
	serverAddressEnv = "PYROSCOPE_SERVER_ADDRESS"
	uploadRateEnv    = "PYROSCOPE_UPLOAD_RATE"
	environmentEnv   = "APP_ENVIRONMENT"
	versionEnv       = "APP_VERSION"
	userFileEnv      = "PYROSCOPE_BASIC_AUTH_USER_FILE"
	passwordFileEnv  = "PYROSCOPE_BASIC_AUTH_PASSWORD_FILE"
	maxSecretBytes   = 8 << 10
	defaultUpload    = 15 * time.Second
)

// StopFunc stops the profiler and flushes its last profile batch. It is safe
// to call more than once.
type StopFunc func()

// StartFromEnv starts profiling for serviceName when
// PYROSCOPE_SERVER_ADDRESS is configured. The endpoint is required to use
// HTTPS, except for localhost development. Remote endpoints require separate
// file-mounted basic-auth credentials. An unset endpoint is a no-op, which
// keeps the same binary safe to deploy before profiling is enabled centrally.
func StartFromEnv(serviceName string) (StopFunc, error) {
	address := strings.TrimSpace(os.Getenv(serverAddressEnv))
	if address == "" {
		return func() {}, nil
	}

	user, err := readSecretFile(userFileEnv)
	if err != nil {
		return nil, err
	}
	password, err := readSecretFile(passwordFileEnv)
	if err != nil {
		return nil, err
	}

	config, err := makeConfig(
		serviceName,
		address,
		strings.TrimSpace(os.Getenv(environmentEnv)),
		strings.TrimSpace(os.Getenv(versionEnv)),
		user,
		password,
		strings.TrimSpace(os.Getenv(uploadRateEnv)),
	)
	if err != nil {
		return nil, err
	}

	profiler, err := pyroscope.Start(config)
	if err != nil {
		return nil, fmt.Errorf("profiling: start Pyroscope client: %w", err)
	}
	return func() { _ = profiler.Stop() }, nil
}

func makeConfig(serviceName, address, environment, version, user, password, uploadRate string) (pyroscope.Config, error) {
	if serviceName == "" || !validLabel(serviceName) {
		return pyroscope.Config{}, fmt.Errorf("profiling: service name must be a non-empty stable identifier")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return pyroscope.Config{}, fmt.Errorf("profiling: server address must be an absolute URL without credentials, query, or fragment")
	}
	local := isLoopback(parsed.Hostname())
	if parsed.Scheme != "https" && !(local && parsed.Scheme == "http") {
		return pyroscope.Config{}, fmt.Errorf("profiling: remote server address must use HTTPS")
	}
	if !local && (user == "" || password == "") {
		return pyroscope.Config{}, fmt.Errorf("profiling: remote server requires file-mounted basic-auth credentials")
	}
	if (user == "") != (password == "") {
		return pyroscope.Config{}, fmt.Errorf("profiling: basic-auth user and password must both be configured")
	}

	rate := defaultUpload
	if uploadRate != "" {
		rate, err = time.ParseDuration(uploadRate)
		if err != nil || rate < 10*time.Second || rate > time.Minute {
			return pyroscope.Config{}, fmt.Errorf("profiling: upload rate must be between 10s and 1m")
		}
	}

	tags := make(map[string]string, 2)
	if environment != "" && validLabel(environment) {
		tags["environment"] = environment
	}
	if version != "" && validLabel(version) {
		tags["version"] = version
	}
	if len(tags) == 0 {
		tags = nil
	}
	return pyroscope.Config{
		ApplicationName:   serviceName,
		ServerAddress:     address,
		BasicAuthUser:     user,
		BasicAuthPassword: password,
		UploadRate:        rate,
		DisableGCRuns:     true,
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
		},
		Tags: tags,
	}, nil
}

func readSecretFile(envName string) (string, error) {
	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("profiling: open %s: %w", envName, err)
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("profiling: read %s: %w", envName, err)
	}
	if len(value) > maxSecretBytes {
		return "", fmt.Errorf("profiling: %s exceeds the %d-byte limit", envName, maxSecretBytes)
	}
	return strings.TrimSpace(string(value)), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validLabel(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}
