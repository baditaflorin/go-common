package apikey

// IssueRequest mirrors the server's issueReq.
type IssueRequest struct {
	User         string `json:"user"`
	TTLSeconds   int64  `json:"ttl_seconds,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Name         string `json:"name,omitempty"`
	Note         string `json:"note,omitempty"`
	NeverExpires bool   `json:"never_expires,omitempty"`
	Key          string `json:"key,omitempty"`       // migration only
	UseLimit     *int64 `json:"use_limit,omitempty"` // nil = unlimited
	// Email tags a key with an owner address so keys issued separately
	// (e.g. one per service) can be grouped as belonging to the same
	// person/account. Purely advisory metadata — not used for auth.
	Email string `json:"email,omitempty"`
	// Tier is the access tier granted to this key (e.g. "open", "free",
	// "vetted-pentest"). Opaque to the keystore — it just stores and
	// echoes it back on /verify; callers decide what tiers mean and
	// enforce them.
	Tier string `json:"tier,omitempty"`
}

// IssueResult is what /issue returns.
type IssueResult struct {
	Key       string `json:"key"`
	User      string `json:"user"`
	Scope     string `json:"scope"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	UseLimit  *int64 `json:"use_limit,omitempty"`
	Email     string `json:"email,omitempty"`
	Tier      string `json:"tier,omitempty"`
}
