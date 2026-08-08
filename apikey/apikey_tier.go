package apikey

// TierSatisfies reports whether a caller holding callerTier may access a
// resource that requires requiredTier.
//
// v1 semantics are deliberately the simplest thing that works: exact
// string equality, fail-closed. There is no hierarchy ("vetted-pentest"
// does NOT imply "free") and no multi-tier-per-key set membership — a
// caller who needs two tiers holds two keys. This avoids inventing tier
// ordering or CSV-set-matching rules nobody has asked for yet; if a real
// need for hierarchy shows up later, it belongs here as one change, not
// scattered across every caller of this function.
//
// requiredTier == ""  → no tier gate; always true, regardless of callerTier.
// requiredTier != ""  → callerTier must equal it exactly. In particular,
// callerTier == "" (a pre-tiering key, or an auth path — local-token
// fast path, private-mesh trust — that never verified a tier at all)
// NEVER satisfies a non-empty requirement. This fail-closed default is
// the point: a bug that leaves callerTier empty must deny, not grant.
func TierSatisfies(callerTier, requiredTier string) bool {
	if requiredTier == "" {
		return true
	}
	return callerTier == requiredTier
}
