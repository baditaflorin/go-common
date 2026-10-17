// Package jsbundle recovers JavaScript source maps from minified bundles.
// Used by error-reporting services to de-obfuscate stack traces.
//
// The bundle URLs come from pages and error reports, so they are untrusted. By
// default (RecoverOptions.Client nil) every fetch goes through a safehttp client
// that refuses internal addresses; supply a client only when you need to reach
// one on purpose, such as an httptest server in a test.
package jsbundle
