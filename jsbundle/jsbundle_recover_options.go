package jsbundle

import (
	"net/http"
)

// RecoverOptions tunes the pipeline.
type RecoverOptions struct {
	Client         *http.Client
	UserAgent      string
	MaxScriptBytes int64
	MaxScripts     int
	MaxConcurrency int
}

func (o *RecoverOptions) normalize() {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.MaxScriptBytes <= 0 {
		o.MaxScriptBytes = DefaultMaxScriptBytes
	}
	if o.MaxScripts <= 0 {
		o.MaxScripts = DefaultMaxScripts
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = DefaultMaxConcurrency
	}
	if o.UserAgent == "" {
		o.UserAgent = "go-common-jsbundle/0.1"
	}
}
