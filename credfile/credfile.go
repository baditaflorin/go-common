package credfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/go-common/env"
)

const (
	// DefaultMaxBytes bounds a key file when Options.MaxBytes is zero.
	DefaultMaxBytes = 8 << 10
	// DefaultCheckInterval is how often a Source looks at its file when
	// Options.CheckInterval is zero.
	DefaultCheckInterval = 30 * time.Second
	// MaxCheckInterval caps Options.CheckInterval: a renewed key is never
	// noticed later than this after its next use.
	MaxCheckInterval = 60 * time.Second
	// DefaultRejectInterval is the shortest gap between two forced re-reads
	// triggered by Rejected, per file, when Options.RejectInterval is zero.
	DefaultRejectInterval = 5 * time.Second
	// MetaSuffix is appended to the key file's path to find its sidecar.
	MetaSuffix = ".meta"
	// ReloadEnv is the process-wide off-switch (see the package doc).
	ReloadEnv = "FLEET_CREDFILE_RELOAD"

	maxMetaBytes = 4 << 10
	// maxRegistered bounds the process-wide registry, so a program that opens
	// many distinct files (a test binary) cannot grow it without limit. A
	// Source opened past the bound works, it is just not in Snapshot.
	maxRegistered = 64
)

// Reasons a read fails, as used in Error.Reason and in the reload error
// counters. The set is fixed so the metric's label stays bounded.
const (
	ReasonMissing      = "missing"       // the file does not exist
	ReasonUnsafe       = "unsafe"        // Protected: not a regular file, a symlink, or group/other permission bits
	ReasonRead         = "read"          // any other I/O failure
	ReasonEmpty        = "empty"         // nothing but whitespace
	ReasonOversized    = "oversized"     // larger than MaxBytes
	ReasonInvalid      = "invalid"       // Options.Validate refused the value
	ReasonMeta         = "meta"          // the .meta sidecar is unreadable or not the documented JSON
	ReasonMetaMismatch = "meta_mismatch" // the sidecar's key_sha256 belongs to another key
)

var envReload = env.Declare(env.Var{
	Name: ReloadEnv, Kind: env.KindString, Default: "on",
	Doc: "Set to off, false, 0, no or disabled to make every file-backed credential (FLEET_FETCH_CACHE_API_KEY_FILE, FLEET_SECRETS_API_KEY_FILE, FLEET_API_KEY_FILE) " +
		"be read once at start and never again, as before go-common 0.112.0. By default a renewed key file is picked up within a minute and after a 401 (package credfile).",
	Check: func(v string) error {
		if _, ok := reloadSetting(v); !ok {
			return errors.New("want on or off")
		}
		return nil
	},
})

// reloadSetting parses FLEET_CREDFILE_RELOAD. Anything unrecognised keeps
// reloading on (the safe default for renewed keys) and is reported by the env
// registry's start-up check.
func reloadSetting(v string) (enabled, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "on", "true", "1", "yes", "enabled":
		return true, true
	case "off", "false", "0", "no", "disabled":
		return false, true
	}
	return true, false
}

// ReloadEnabled reports whether FLEET_CREDFILE_RELOAD leaves reloading on.
func ReloadEnabled() bool {
	on, _ := reloadSetting(envReload.Value())
	return on
}

// Options tune a Source. The zero value reads any readable file of up to
// DefaultMaxBytes and checks it every DefaultCheckInterval.
type Options struct {
	// Protected requires a regular file, not a symlink, with no group or other
	// permission bits (0400 or 0600), checked on every read.
	Protected bool
	// MaxBytes bounds the file; 0 means DefaultMaxBytes.
	MaxBytes int
	// Validate, when set, is applied to every value read. A value it refuses
	// fails the read with ReasonInvalid, and the last good value stays. Its
	// error text is never repeated (it might quote the value).
	Validate func(string) error
	// CheckInterval is the least time between two looks at the file; 0 means
	// DefaultCheckInterval, and it is capped at MaxCheckInterval.
	CheckInterval time.Duration
	// RejectInterval is the least time between two forced re-reads by
	// Rejected; 0 means DefaultRejectInterval.
	RejectInterval time.Duration
}

// Error is a failed read. It names the credential, the file and the reason,
// never the value.
type Error struct {
	Name   string
	Path   string
	Reason string // one of the Reason constants
	err    error  // an I/O error, for errors.Is; never a Validate error
}

func (e *Error) Error() string {
	return fmt.Sprintf("credfile: %s file %s: %s", e.Name, e.Path, e.Reason)
}

// Unwrap returns the underlying I/O error, if any, so errors.Is(err,
// fs.ErrNotExist) works.
func (e *Error) Unwrap() error { return e.err }

// state is the value a Source hands out, swapped atomically.
type state struct {
	value string
	gen   uint64
	bad   bool // the last look at the file failed; value is the last good one
}

// Source is a file-backed credential. It is safe for concurrent use.
type Source struct {
	name, path string
	opts       Options
	reload     bool
	now        func() time.Time

	st        atomic.Pointer[state]
	nextCheck atomic.Int64 // unix nanoseconds

	reloadOK  atomic.Uint64
	reloadErr atomic.Uint64
	staleUses atomic.Uint64

	mu            sync.Mutex // serialises looks at the file; guards the fields below
	seen          os.FileInfo
	metaSeen      os.FileInfo
	keySum        string
	meta          Meta
	hasMeta       bool
	mismatchSince time.Time
	mismatchCount bool
	lastForced    time.Time
	errs          map[string]uint64
}

var (
	regMu    sync.Mutex
	registry = map[string]*Source{}
)

func registryKey(name, path string, opts Options) string {
	return fmt.Sprintf("%s\x00%s\x00%t\x00%d", name, path, opts.Protected, opts.MaxBytes)
}

// Open reads the credential name from path and returns a Source for it. The
// first read happens here: when it fails, Open returns the *Error and no
// Source, so a service fails at start exactly as it did when it read the file
// itself.
//
// Sources are shared: a second Open of the same name and path (with the same
// Protected and MaxBytes) returns the first Source, so two packages reading
// one file count its reloads once. name labels the metrics; use the file's
// conventional name (fleet_api_key, fleet_secrets_api_key, ...), and the same
// name only for the same use (the first Open's Validate applies).
func Open(name, path string, opts Options) (*Source, error) {
	name, path = strings.TrimSpace(name), strings.TrimSpace(path)
	if name == "" || path == "" {
		return nil, errors.New("credfile: Open needs a name and a path")
	}
	key := registryKey(name, path, opts)
	regMu.Lock()
	existing := registry[key]
	regMu.Unlock()
	if existing != nil {
		existing.maybeCheck()
		return existing, nil
	}

	s := newSource(name, path, opts, time.Now)
	if err := s.load(); err != nil {
		return nil, err
	}
	regMu.Lock()
	defer regMu.Unlock()
	if other := registry[key]; other != nil {
		return other, nil // another goroutine opened it first
	}
	if len(registry) < maxRegistered {
		registry[key] = s
	}
	return s, nil
}

func newSource(name, path string, opts Options, now func() time.Time) *Source {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = DefaultCheckInterval
	}
	if opts.CheckInterval > MaxCheckInterval {
		opts.CheckInterval = MaxCheckInterval
	}
	if opts.RejectInterval <= 0 {
		opts.RejectInterval = DefaultRejectInterval
	}
	return &Source{name: name, path: path, opts: opts, reload: ReloadEnabled(), now: now, errs: map[string]uint64{}}
}

// load is the first read, at Open.
func (s *Source) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, info, err := s.readKey()
	if err != nil {
		return err
	}
	s.seen = info
	s.keySum = KeySHA256(value)
	s.st.Store(&state{value: value, gen: 1})
	now := s.now()
	s.checkMetaLocked(now, true)
	s.nextCheck.Store(now.Add(s.opts.CheckInterval).UnixNano())
	return nil
}

// Name is the credential's name, as given to Open.
func (s *Source) Name() string { return s.name }

// Path is the key file's path.
func (s *Source) Path() string { return s.path }

// String names the Source without its value, so a Source printed by mistake
// leaks nothing.
func (s *Source) String() string { return "credfile.Source(" + s.name + ", " + s.path + ")" }

// GoString is String, for %#v.
func (s *Source) GoString() string { return s.String() }

// Value returns the current key: the file's content as of the last look, or
// the last good content when the file has since become unreadable (each such
// use is counted as stale). Never log it.
func (s *Source) Value() string {
	v, _ := s.Current()
	return v
}

// Current is Value plus a generation number that changes whenever the value
// does, so a caller can tell whether the key it was refused with is still
// the current one.
func (s *Source) Current() (string, uint64) {
	s.maybeCheck()
	st := s.st.Load()
	if st.bad {
		s.staleUses.Add(1)
	}
	return st.value, st.gen
}

// Rejected is called after the callee refused the key used (an HTTP 401). It
// re-reads the file at once, at most once per RejectInterval, and returns the
// current value and whether it differs from used. Retry the request once when
// changed is true; when it is false, the file holds nothing better, so do not
// retry. With reloading off it never re-reads.
func (s *Source) Rejected(used string) (value string, changed bool) {
	if s.reload {
		s.mu.Lock()
		now := s.now()
		if s.lastForced.IsZero() || now.Sub(s.lastForced) >= s.opts.RejectInterval {
			s.lastForced = now
			s.checkLocked(now, true)
		}
		s.mu.Unlock()
	}
	st := s.st.Load()
	return st.value, st.value != used
}

// Expiry is the key's expiry from the .meta sidecar, if it has one that
// matches the key.
func (s *Source) Expiry() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiryLocked()
}

func (s *Source) expiryLocked() (time.Time, bool) {
	if !s.hasMeta || s.meta.ExpiresAt.IsZero() {
		return time.Time{}, false
	}
	return s.meta.ExpiresAt, true
}

func (s *Source) maybeCheck() {
	if !s.reload {
		return
	}
	now := s.now()
	if now.UnixNano() < s.nextCheck.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.UnixNano() < s.nextCheck.Load() {
		return // another goroutine looked meanwhile
	}
	s.checkLocked(now, false)
}

// checkLocked looks at the file and re-reads it when it changed (or when
// force is set). s.mu must be held.
func (s *Source) checkLocked(now time.Time, force bool) {
	s.nextCheck.Store(now.Add(s.opts.CheckInterval).UnixNano())
	info, err := s.statKey()
	if err != nil {
		s.seen = nil // re-read whatever appears next
		s.failLocked(err)
		return
	}
	if !force && s.seen != nil && sameFile(s.seen, info) {
		s.checkMetaLocked(now, false)
		return
	}
	value, rinfo, err := s.readKey()
	if err != nil {
		var e *Error
		if errors.As(err, &e) && (e.Reason == ReasonMissing || e.Reason == ReasonRead) {
			s.seen = nil
		} else {
			// The file is there but its content is unusable: do not read it
			// again until it changes. Stale uses keep counting meanwhile.
			s.seen = info
		}
		s.failLocked(err)
		return
	}
	cur := s.st.Load()
	next := &state{value: value, gen: cur.gen}
	if value != cur.value {
		next.gen++
		s.keySum = KeySHA256(value)
	}
	s.st.Store(next)
	s.seen = rinfo
	s.reloadOK.Add(1)
	s.checkMetaLocked(now, true)
}

func (s *Source) failLocked(err error) {
	reason := ReasonRead
	var e *Error
	if errors.As(err, &e) {
		reason = e.Reason
	}
	s.reloadErr.Add(1)
	s.errs[reason]++
	if cur := s.st.Load(); cur != nil && !cur.bad {
		s.st.Store(&state{value: cur.value, gen: cur.gen, bad: true})
	}
}

func (s *Source) newErr(reason string, err error) *Error {
	return &Error{Name: s.name, Path: s.path, Reason: reason, err: err}
}

func (s *Source) statKey() (os.FileInfo, error) {
	var info os.FileInfo
	var err error
	if s.opts.Protected {
		info, err = os.Lstat(s.path)
	} else {
		info, err = os.Stat(s.path)
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, s.newErr(ReasonMissing, err)
		}
		return nil, s.newErr(ReasonRead, err)
	}
	if s.opts.Protected && (!info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0) {
		return nil, s.newErr(ReasonUnsafe, nil)
	}
	return info, nil
}

// readKey reads and validates the key. The returned FileInfo is that of the
// file actually read (fstat of the open descriptor), so a rename between the
// stat and the open cannot pair new content with an old identity.
func (s *Source) readKey() (string, os.FileInfo, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		pre, err := s.statKey()
		if err != nil {
			return "", nil, err
		}
		f, err := os.Open(s.path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", nil, s.newErr(ReasonMissing, err)
			}
			return "", nil, s.newErr(ReasonRead, err)
		}
		info, statErr := f.Stat()
		data, readErr := io.ReadAll(io.LimitReader(f, int64(s.opts.MaxBytes)+1))
		_ = f.Close()
		if statErr != nil || readErr != nil {
			return "", nil, s.newErr(ReasonRead, errors.Join(statErr, readErr))
		}
		if s.opts.Protected && !os.SameFile(pre, info) {
			// Replaced between the permission check and the open: look again.
			lastErr = s.newErr(ReasonRead, errors.New("file replaced during read"))
			continue
		}
		if len(data) > s.opts.MaxBytes {
			return "", info, s.newErr(ReasonOversized, nil)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", info, s.newErr(ReasonEmpty, nil)
		}
		if s.opts.Validate != nil && s.opts.Validate(value) != nil {
			return "", info, s.newErr(ReasonInvalid, nil)
		}
		return value, info, nil
	}
	return "", nil, lastErr
}

func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
}

// checkMetaLocked reads the sidecar when it changed, or always when force is
// set (the key changed, so a mismatch may have resolved).
func (s *Source) checkMetaLocked(now time.Time, force bool) {
	metaPath := s.path + MetaSuffix
	info, err := os.Stat(metaPath)
	if err != nil {
		s.metaSeen, s.hasMeta, s.meta = nil, false, Meta{}
		s.mismatchSince, s.mismatchCount = time.Time{}, false
		if !errors.Is(err, fs.ErrNotExist) {
			s.errs[ReasonMeta]++
		}
		return
	}
	if !force && s.metaSeen != nil && sameFile(s.metaSeen, info) {
		s.noteMismatchLocked(now)
		return
	}
	s.metaSeen = info
	s.hasMeta, s.meta = false, Meta{}
	s.mismatchSince, s.mismatchCount = time.Time{}, false
	if !info.Mode().IsRegular() || info.Size() > maxMetaBytes {
		s.errs[ReasonMeta]++
		return
	}
	data, err := os.ReadFile(metaPath)
	if err != nil || len(data) > maxMetaBytes {
		s.errs[ReasonMeta]++
		return
	}
	m, err := ParseMeta(data)
	if err != nil {
		s.errs[ReasonMeta]++
		return
	}
	if m.KeySHA256 != "" && !strings.EqualFold(m.KeySHA256, s.keySum) {
		// The key and its sidecar are two renames: between them a reader sees
		// a pair that does not match. Count it only if it outlives one check
		// interval, so a normal renewal is not an error.
		s.mismatchSince = now
		return
	}
	s.meta, s.hasMeta = m, true
}

func (s *Source) noteMismatchLocked(now time.Time) {
	if s.mismatchSince.IsZero() || s.mismatchCount {
		return
	}
	if now.Sub(s.mismatchSince) >= s.opts.CheckInterval {
		s.mismatchCount = true
		s.errs[ReasonMetaMismatch]++
	}
}

// KeySHA256 is the sidecar's key_sha256 for value: lowercase hex SHA-256 of
// the trimmed key. Writers put it in the .meta so readers can tell a sidecar
// that belongs to another key.
func KeySHA256(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

// Stats are a Source's counters. They carry no value.
type Stats struct {
	Name, Path string
	// Expiry is the key's expiry from a matching .meta; zero when unknown.
	Expiry time.Time
	// ReloadOK counts re-reads after start that succeeded, ReloadError looks
	// at the file that failed (by reason in Errors, which also counts sidecar
	// problems that do not fail the key).
	ReloadOK, ReloadError uint64
	Errors                map[string]uint64
	// StaleUses counts values handed out while the file was unreadable.
	StaleUses uint64
	// Stale is true while the last look at the file failed.
	Stale      bool
	Generation uint64
}

// Stats returns the Source's counters.
func (s *Source) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st.Load()
	out := Stats{
		Name: s.name, Path: s.path,
		ReloadOK: s.reloadOK.Load(), ReloadError: s.reloadErr.Load(), StaleUses: s.staleUses.Load(),
		Errors: make(map[string]uint64, len(s.errs)), Stale: st.bad, Generation: st.gen,
	}
	if exp, ok := s.expiryLocked(); ok {
		out.Expiry = exp
	}
	for k, v := range s.errs {
		out.Errors[k] = v
	}
	return out
}

// Snapshot looks at every registered file that is due a look (so a scrape
// sees a renewed key's expiry even when the service is idle) and returns the
// Stats of each, in no particular order.
func Snapshot() []Stats {
	regMu.Lock()
	sources := make([]*Source, 0, len(registry))
	for _, s := range registry {
		sources = append(sources, s)
	}
	regMu.Unlock()
	out := make([]Stats, 0, len(sources))
	for _, s := range sources {
		s.maybeCheck()
		out = append(out, s.Stats())
	}
	return out
}
