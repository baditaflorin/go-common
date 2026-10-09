// Package profiling provides opt-in continuous Go profiling through Grafana
// Pyroscope. Importing this package has no runtime effect until a server
// address is configured.
package profiling

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/go-common/secrets"
	pyroscope "github.com/grafana/pyroscope-go"
)

const (
	serverAddressEnv  = "PYROSCOPE_SERVER_ADDRESS"
	uploadRateEnv     = "PYROSCOPE_UPLOAD_RATE"
	environmentEnv    = "APP_ENVIRONMENT"
	versionEnv        = "APP_VERSION"
	userFileEnv       = "PYROSCOPE_BASIC_AUTH_USER_FILE"
	passwordFileEnv   = "PYROSCOPE_BASIC_AUTH_PASSWORD_FILE"
	userSecretEnv     = "PYROSCOPE_BASIC_AUTH_USER_SECRET"
	passwordSecretEnv = "PYROSCOPE_BASIC_AUTH_PASSWORD_SECRET"
	maxSecretBytes    = 8 << 10
	defaultUpload     = 15 * time.Second
)

// StopFunc stops the profiler and flushes its last profile batch. It is safe
// to call more than once.
type StopFunc func()

var processProfiler struct {
	sync.Mutex
	serviceName string
	stop        StopFunc
}

var startProfiler = func(config pyroscope.Config) (StopFunc, error) {
	profiler, err := pyroscope.Start(config)
	if err != nil {
		return nil, err
	}
	return func() { _ = profiler.Stop() }, nil
}

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

	processProfiler.Lock()
	defer processProfiler.Unlock()
	if processProfiler.stop != nil {
		if processProfiler.serviceName != serviceName {
			return nil, fmt.Errorf("profiling: already started for service %q", processProfiler.serviceName)
		}
		return processProfiler.stop, nil
	}

	user, password, err := loadBasicAuth(os.Getenv, func(name string) (string, error) {
		client, err := secrets.NewFromEnv(os.Getenv, nil)
		if err != nil {
			return "", err
		}
		return client.Get(context.Background(), name)
	})
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

	stopProfiler, err := startProfiler(config)
	if err != nil {
		return nil, fmt.Errorf("profiling: start Pyroscope client: %w", err)
	}
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(stopProfiler) }
	processProfiler.serviceName = serviceName
	processProfiler.stop = stop
	return stop, nil
}

// loadBasicAuth resolves either protected file mounts or service-scoped
// Fleet Secrets names. Secret lookup is lazy: services without the profiling
// endpoint configured do not need vault access.
func loadBasicAuth(getenv func(string) string, getSecret func(string) (string, error)) (string, string, error) {
	userSecret := strings.TrimSpace(getenv(userSecretEnv))
	passwordSecret := strings.TrimSpace(getenv(passwordSecretEnv))
	userFile := strings.TrimSpace(getenv(userFileEnv))
	passwordFile := strings.TrimSpace(getenv(passwordFileEnv))
	if (userSecret == "") != (passwordSecret == "") || (userFile == "") != (passwordFile == "") {
		return "", "", fmt.Errorf("profiling: basic-auth user and password must both be configured")
	}
	if (userSecret != "" || passwordSecret != "") && (userFile != "" || passwordFile != "") {
		return "", "", fmt.Errorf("profiling: configure basic-auth credentials using either secret names or files")
	}
	if userSecret == "" && passwordSecret == "" && userFile == "" && passwordFile == "" {
		return "", "", nil
	}
	if userSecret != "" {
		if getSecret == nil {
			return "", "", fmt.Errorf("profiling: Fleet Secrets reader is unavailable")
		}
		user, err := getSecret(userSecret)
		if err != nil {
			return "", "", fmt.Errorf("profiling: could not load basic-auth user from Fleet Secrets")
		}
		password, err := getSecret(passwordSecret)
		if err != nil {
			return "", "", fmt.Errorf("profiling: could not load basic-auth password from Fleet Secrets")
		}
		return strings.TrimSpace(user), strings.TrimSpace(password), nil
	}
	user, err := readSecretFilePath(userFile)
	if err != nil {
		return "", "", fmt.Errorf("profiling: could not load basic-auth user file")
	}
	password, err := readSecretFilePath(passwordFile)
	if err != nil {
		return "", "", fmt.Errorf("profiling: could not load basic-auth password file")
	}
	return user, password, nil
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
	return readSecretFilePath(path)
}

func readSecretFilePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("profiling: secret file is unavailable or has unsafe permissions")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("profiling: secret file could not be opened")
	}
	defer f.Close()
	value, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("profiling: secret file could not be read")
	}
	if len(value) > maxSecretBytes {
		return "", fmt.Errorf("profiling: secret file exceeds the %d-byte limit", maxSecretBytes)
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
