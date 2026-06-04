package apikey

// IssueRequest mirrors the server's issueReq.
type IssueRequest struct {
	User         string `json:"user"`
	TTLSeconds   int64  `json:"ttl_seconds,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Note         string `json:"note,omitempty"`
	NeverExpires bool   `json:"never_expires,omitempty"`
	Key          string `json:"key,omitempty"` // migration only
}

// IssueResult is what /issue returns.
type IssueResult struct {
	Key       string `json:"key"`
	User      string `json:"user"`
	Scope     string `json:"scope"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}
