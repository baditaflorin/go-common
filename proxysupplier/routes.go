package proxysupplier

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// RouteIntent is an explicit request-level routing preference. Set at most one
// of ProfileID, ProviderID, or PoolID. A route router never silently ignores a
// supplied selector or falls back to the process default.
type RouteIntent struct {
	ProfileID  string
	ProviderID string
	PoolID     string
	Class      PoolClass
}

// RouteCandidate is safe metadata describing one configured exact route.
// ProfileID is set when the candidate came from a named ordered profile.
type RouteCandidate struct {
	ProfileID  string
	ProviderID string
	PoolID     string
	AccountID  string
	Class      PoolClass
}

// RouteOutcome describes the observed result for one exact selected route.
type RouteOutcome uint8

const (
	RouteOutcomeSuccess RouteOutcome = iota + 1
	RouteOutcomeTransportFailure
	RouteOutcomeChallenge
	RouteOutcomeOriginResponse
	RouteOutcomeCanceled
)

// RouteSelector resolves explicit route intents and provides exact-selection
// feedback. It is an optional capability; the legacy Supplier API is unchanged.
type RouteSelector interface {
	ResolveCandidates(intent RouteIntent) ([]RouteCandidate, error)
	SelectRoute(candidate RouteCandidate, allowResidential bool) (RouteSelection, error)
	MarkRouteResult(selection RouteSelection, outcome RouteOutcome)
}

// RouteSelection is an opaque exact route. Only safe labels are exported;
// credentials and the proxy URL remain private until ProxyURL is explicitly
// called by a transport constructor.
type RouteSelection struct {
	descriptor RouteCandidate
	proxyURL   string
	owner      *RouteRouter
	provider   routeProvider
	token      any
}

// Descriptor returns the non-secret labels for the selected route.
func (s RouteSelection) Descriptor() RouteCandidate { return s.descriptor }

// Report records a result against the exact provider route that created this
// selection. It is safe to call more than once and is a no-op for an invalid or
// closed selection.
func (s RouteSelection) Report(outcome RouteOutcome) {
	if s.owner != nil {
		s.owner.MarkRouteResult(s, outcome)
	}
}

// ProxyURL returns a parsed copy of the credential-bearing proxy URL. Callers
// should pass it directly to a transport and must not log it.
func (s RouteSelection) ProxyURL() (*url.URL, error) {
	if s.proxyURL == "" || s.owner == nil || s.provider == nil {
		return nil, ErrProxyUnavailable
	}
	u, err := url.Parse(s.proxyURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Fragment != "" {
		return nil, ErrProxyUnavailable
	}
	return u, nil
}

func (s RouteSelection) String() string {
	return fmt.Sprintf("RouteSelection{ProfileID:%q ProviderID:%q PoolID:%q AccountID:%q Class:%q}",
		s.descriptor.ProfileID, s.descriptor.ProviderID, s.descriptor.PoolID, s.descriptor.AccountID, s.descriptor.Class)
}

func (s RouteSelection) GoString() string { return s.String() }

// ParseRouteIntentQuery parses the supported request query selectors. The
// selector parameters are mutually exclusive; proxy_class is meaningful only
// with proxy_provider. The parser is intentionally separate from policy: a
// caller must still obtain any required residential permit before selection.
func ParseRouteIntentQuery(q url.Values) (RouteIntent, error) {
	var intent RouteIntent
	poolID, poolSet, err := singleRouteQueryValue(q, "proxy_pool")
	if err != nil {
		return intent, err
	}
	providerID, providerSet, err := singleRouteQueryValue(q, "proxy_provider")
	if err != nil {
		return intent, err
	}
	profileID, profileSet, err := singleRouteQueryValue(q, "proxy_profile")
	if err != nil {
		return intent, err
	}
	classText, classSet, err := singleRouteQueryValue(q, "proxy_class")
	if err != nil {
		return intent, err
	}
	if countRouteSelectors(poolSet, providerSet, profileSet) > 1 || (classSet && !providerSet) {
		return intent, errors.New("proxysupplier: conflicting route selectors")
	}
	if classSet && classText != string(PoolClassDC) && classText != string(PoolClassResidential) {
		return intent, errors.New("proxysupplier: invalid route class")
	}
	for _, id := range []string{poolID, providerID, profileID} {
		if id != "" && !webshareSafeID.MatchString(id) {
			return intent, errors.New("proxysupplier: invalid route selector")
		}
	}
	if poolSet {
		intent.PoolID = poolID
	}
	if providerSet {
		intent.ProviderID = providerID
		intent.Class = PoolClass(classText)
	}
	if profileSet {
		intent.ProfileID = profileID
	}
	return intent, nil
}

func singleRouteQueryValue(q url.Values, key string) (string, bool, error) {
	values, ok := q[key]
	if !ok {
		return "", false, nil
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", true, errors.New("proxysupplier: invalid route selector")
	}
	return strings.TrimSpace(values[0]), true, nil
}

func countRouteSelectors(pool, provider, profile bool) int {
	n := 0
	for _, present := range []bool{pool, provider, profile} {
		if present {
			n++
		}
	}
	return n
}
