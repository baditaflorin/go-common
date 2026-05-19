// Package policyeval is a pure-Go rule engine for in-process policy
// evaluation. Rules are declared as Go literals (no YAML/JSON files at
// runtime), executed against a map[string]any Fact, and return the
// winning Decision plus an explanation string.
package policyeval
