package promxtest

import (
	"fmt"
	"strings"
	"sync"
)

// Inputs is the hostile corpus a Probe draws from: the values someone who can
// reach the process could choose for everything a collector might turn into a
// label. Every pool is deterministic, so a run is repeatable and the series
// count it produces is a number a budget can name.
//
// A pool holds Distinct values, no two equal, and a pool mixes shapes: plain
// values (most of them, so a label that keeps the first values it sees fills up
// with them), upper-case variants, unicode (a right-to-left override, a zero-width
// space, accents, CJK, an emoji), values that are not valid UTF-8, values just over
// and far over 256 bytes (one of 1 MiB, as long as a request line Go's server
// accepts), and values with NUL, CR, LF, quotes and braces in them, which break
// anything that writes a label without escaping it. The empty string is the first
// value of every pool.
type Inputs struct {
	// Distinct is how many distinct values each pool holds (2000 times the scale).
	// It has to be past every limit the collector under test applies, or the
	// collector would look bounded only because the input ran out.
	Distinct int
	// Burst and Repeat shape Each: the first Burst values of a pool are repeated
	// Repeat times in a row after the pass over all of them. 80 repeats is past the
	// default of 64 that a heavy-hitter rule wants before it names a value.
	Burst, Repeat int

	mu    sync.Mutex
	pools map[string][]string
}

// NewInputs returns the corpus at a scale: scale 1 is 2000 distinct values per
// pool, scale 4 is 8000. AssertBounded runs a collector at both and fails when the
// larger run creates more series than the smaller. A scale below 1 is 1.
func NewInputs(scale int) *Inputs {
	scale = max(scale, 1)
	return &Inputs{Distinct: 2000 * scale, Burst: 150, Repeat: 80, pools: map[string][]string{}}
}

// Each calls fn for every value in values the way a hostile client would feed
// them: first one pass in which every value is new (a flood), which is what an
// unbounded label dies of, then the first Burst values Repeat times each in a row,
// which is the worst case for a rule that names a value after enough hits (every
// one of them qualifies, so a rule that admits too many shows it).
func (in *Inputs) Each(values []string, fn func(v string)) {
	for _, v := range values {
		fn(v)
	}
	for _, v := range values[:min(in.Burst, len(values))] {
		for range in.Repeat {
			fn(v)
		}
	}
}

// Hosts returns hostnames and host-like values: names, IPv4 and bracketed IPv6
// addresses, punycode, names with a port or a trailing dot.
func (in *Inputs) Hosts() []string {
	return in.pool("hosts", nil, func(i int) string {
		switch i % 5 {
		case 1:
			return fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)
		case 2:
			return fmt.Sprintf("[2001:db8::%x]", i)
		case 3:
			return fmt.Sprintf("xn--h%06d-9ca.example", i)
		case 4:
			return fmt.Sprintf("h%06d.attacker.example.:%d", i, 1024+i%60000)
		}
		return fmt.Sprintf("h%06d.attacker.example", i)
	})
}

// Methods returns HTTP method tokens: the standard ones, WebDAV's, lower-case
// variants, and more distinct tokens than any list could hold.
func (in *Inputs) Methods() []string {
	standard := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE", "PROPFIND", "MKCOL", "get"}
	return in.pool("methods", standard, func(i int) string { return fmt.Sprintf("M%06d", i) })
}

// Paths returns URL paths: parameterised ones (ids, uuids), traversals, and the
// decoded form of percent escapes that are not UTF-8 (GET /%ff is "/\xff").
func (in *Inputs) Paths() []string {
	return in.pool("paths", nil, func(i int) string {
		switch i % 4 {
		case 1:
			return fmt.Sprintf("/%08x-%04x-%04x-%04x-%012x", i, i&0xffff, i>>4&0xffff, i>>8&0xffff, i)
		case 2:
			return fmt.Sprintf("/../../etc/passwd/%d", i)
		case 3:
			return fmt.Sprintf("//a/%d//b/", i)
		}
		return fmt.Sprintf("/users/%d/orders/%d", i, i*7)
	})
}

// Schemes returns URL schemes: the two that matter, others that a URL parser
// accepts (a letter, then letters, digits, + - .), and more of them than any list
// could hold.
func (in *Inputs) Schemes() []string {
	known := []string{"http", "https", "HTTP", "ftp", "gopher", "file", "ws", "wss", "javascript", "data"}
	return in.pool("schemes", known, func(i int) string { return fmt.Sprintf("s%06d+x.y-z", i) })
}

// UserAgents returns User-Agent header values, the usual shape of one plus the
// hostile shapes above.
func (in *Inputs) UserAgents() []string {
	return in.pool("useragents", nil, func(i int) string {
		return fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) bot-%d/1.0 (+https://attacker.example/%d)", i, i)
	})
}

// Names returns free-form identifiers: what a caller might pass as a name,
// a reason, a primitive or a token when it builds that string from input.
func (in *Inputs) Names() []string {
	return in.pool("names", nil, func(i int) string { return fmt.Sprintf("n%06d", i) })
}

// Statuses returns every status code Go's HTTP server accepts, 100 to 999: a
// handler that relays an upstream's status can be made to write any of them.
func (in *Inputs) Statuses() []int {
	s := make([]int, 0, 900)
	for code := 100; code <= 999; code++ {
		s = append(s, code)
	}
	return s
}

// pool builds the pool for kind once. fixed are values that must appear as they
// are (the standard methods, the schemes that matter): they take indexes 1 to
// len(fixed), after the empty string at 0, and escape hostile.
func (in *Inputs) pool(kind string, fixed []string, base func(i int) string) []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	if p, ok := in.pools[kind]; ok {
		return p
	}
	p := make([]string, in.Distinct)
	for i := range p {
		p[i] = hostile(i, base(i))
	}
	if len(p) > 19 {
		p[19] = base(19) + strings.Repeat("z", 1<<20) // as long as a request line can be
	}
	copy(p[1:], fixed)
	if len(p) > 0 {
		p[0] = ""
	}
	in.pools[kind] = p
	return p
}

// hostile turns base, which is unique to i, into one of the shapes a label has to
// survive. Four in ten stay plain.
func hostile(i int, base string) string {
	switch i % 10 {
	case 4:
		return strings.ToUpper(base)
	case 5:
		return base + "\u202e\u200b é日本語🙂"
	case 6:
		return "\xff\xfe" + base // not valid UTF-8: client_golang panics on it
	case 7:
		return base + strings.Repeat("x", 300)
	case 8:
		return base + "\x00\r\n\"\\{}label=\"v\""
	case 9:
		if i%500 == 9 {
			return base + strings.Repeat("y", 64<<10)
		}
		return base + strings.Repeat("y", 1000)
	}
	return base
}
