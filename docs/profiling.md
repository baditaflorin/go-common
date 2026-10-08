# Opt-in Go profiling

`profiling.StartFromEnv(serviceName)` starts the official Pyroscope Go SDK when
`PYROSCOPE_SERVER_ADDRESS` is set. An empty address returns a safe no-op, so
service binaries can adopt the package before a central profile endpoint is
enabled.

Remote endpoints must use HTTPS and file-mounted HTTP Basic credentials:

```text
PYROSCOPE_SERVER_ADDRESS=https://profiles.example.com
PYROSCOPE_BASIC_AUTH_USER_FILE=/run/secrets/pyroscope_user
PYROSCOPE_BASIC_AUTH_PASSWORD_FILE=/run/secrets/pyroscope_password
PYROSCOPE_UPLOAD_RATE=15s
APP_ENVIRONMENT=production
APP_VERSION=1.2.3
```

Local development may use an unauthenticated loopback HTTP endpoint. Credentials
are read only from the paths in the two `_FILE` variables, are size-bounded, and
are never included in errors.

The client captures CPU, allocated bytes, in-use bytes, and goroutine profiles.
It uploads at a bounded interval (15 seconds by default; 10 seconds to 1 minute
allowed) and does not force garbage collection while collecting heap profiles.
This reduces profiler-driven pauses while making heap samples reflect the
runtime's current collection state. It does not enable mutex or block sampling,
which change process-wide runtime sampling rates.

Each adopter should start the profiler after service identity/configuration is
available and defer the returned stop function. Do not expose `net/http/pprof`
on a public listener. The profiler sends outbound to the configured endpoint;
it does not create an HTTP listener.
