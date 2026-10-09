# Opt-in Go profiling

The shared `server.Server.Start` lifecycle starts the official Pyroscope Go
SDK automatically when `PYROSCOPE_SERVER_ADDRESS` is set, using the service's
stable public slug and stopping the profiler during shutdown. An empty address
returns a safe no-op, so binaries can adopt this library release before a
central profile endpoint is enabled. Services with custom server lifecycles
can call `profiling.StartFromEnv(serviceName)` themselves; both paths share one
idempotent process-wide profiler.

Remote endpoints must use HTTPS and HTTP Basic credentials. Credentials can be
mounted as protected files:

```text
PYROSCOPE_SERVER_ADDRESS=https://profiles.example.com
PYROSCOPE_BASIC_AUTH_USER_FILE=/run/secrets/pyroscope_user
PYROSCOPE_BASIC_AUTH_PASSWORD_FILE=/run/secrets/pyroscope_password
PYROSCOPE_UPLOAD_RATE=15s
APP_ENVIRONMENT=production
APP_VERSION=1.2.3
```

For centralized opt-in, use service-allowlisted Fleet Secrets names instead:

```text
PYROSCOPE_BASIC_AUTH_USER_SECRET=pyroscope-company-size-user
PYROSCOPE_BASIC_AUTH_PASSWORD_SECRET=pyroscope-company-size-password
FLEET_SECRETS_API_KEY_FILE=/run/secrets/fleet_secrets_api_key
```

Secret-name mode uses the service's existing protected Fleet Secrets API key
file and the approved Fleet Secrets HTTPS endpoint. Never put secret values in
environment variables. File and secret-name modes cannot be mixed. Local
development may use an unauthenticated loopback HTTP endpoint. File credentials
are size-bounded, and values are never included in errors.

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
