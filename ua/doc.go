// Package ua builds the fleet User-Agent header value.
//
// Every fleet service should brand its outbound HTTP calls:
//
//	client := safehttp.NewClient(safehttp.WithUserAgent(ua.Build(ServiceID, Version)))
//
// This produces "go_myservice/v1.2.3 (+https://github.com/baditaflorin/go_myservice)".
package ua
