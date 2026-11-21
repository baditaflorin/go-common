package proxysupplier

import (
	"net/netip"
	"sort"
)

// subnet24ForHost derives a bounded subnet identifier from a proxy host string:
// IPv4 addresses map to their /24 prefix (e.g. "104.239.112.0/24"), IPv6
// addresses map to their /48 prefix. Invalid or non-IP hosts return "".
func subnet24ForHost(host string) string {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return netip.PrefixFrom(addr, 24).Masked().String()
	}
	return netip.PrefixFrom(addr, 48).Masked().String()
}

// interleaveWebshareEntries orders proxy entries so consecutive round-robin
// selections spread across ASNs and /24 subnets proportionally to each group's
// size (two-level smooth weighted round-robin), instead of walking adjacent IPs
// inside the same /24 and ASN back-to-back.
func interleaveWebshareEntries(entries []*webshareDirectEntry) []*webshareDirectEntry {
	if len(entries) <= 1 {
		out := make([]*webshareDirectEntry, len(entries))
		copy(out, entries)
		return out
	}

	// 1. Group entries by ASN (0 = unknown ASN), sorted deterministically.
	byASN := make(map[int][]*webshareDirectEntry)
	for _, e := range entries {
		byASN[e.asnNumber] = append(byASN[e.asnNumber], e)
	}
	asnKeys := make([]int, 0, len(byASN))
	for asn := range byASN {
		asnKeys = append(asnKeys, asn)
	}
	sort.Ints(asnKeys)

	type bucket struct {
		items []*webshareDirectEntry
		pos   int
	}

	// 2. Within each ASN, interleave its /24 subnets using smooth weighted RR.
	asnBuckets := make([]*bucket, 0, len(asnKeys))
	for _, asn := range asnKeys {
		interleaved := interleaveBySubnet(byASN[asn])
		asnBuckets = append(asnBuckets, &bucket{items: interleaved})
	}

	// 3. Across ASNs, emit entries by picking the ASN with the most remaining
	// entries while avoiding repeating the previous ASN whenever possible.
	// This guarantees zero adjacent same-ASN entries whenever no ASN holds
	// >50% of entries, and spreads big ASNs evenly across the entire cycle.
	out := make([]*webshareDirectEntry, 0, len(entries))
	lastASN := -1
	for len(out) < len(entries) {
		bestIdx := -1
		bestRem := 0
		for i, b := range asnBuckets {
			rem := len(b.items) - b.pos
			if rem == 0 {
				continue
			}
			if i == lastASN {
				continue
			}
			if rem > bestRem {
				bestRem = rem
				bestIdx = i
			}
		}
		if bestIdx == -1 {
			// Only the previous ASN still has remaining entries.
			if lastASN >= 0 && asnBuckets[lastASN].pos < len(asnBuckets[lastASN].items) {
				bestIdx = lastASN
			} else {
				break
			}
		}
		chosen := asnBuckets[bestIdx]
		out = append(out, chosen.items[chosen.pos])
		chosen.pos++
		lastASN = bestIdx
	}
	return out
}

func interleaveBySubnet(entries []*webshareDirectEntry) []*webshareDirectEntry {
	if len(entries) <= 1 {
		out := make([]*webshareDirectEntry, len(entries))
		copy(out, entries)
		return out
	}
	bySub := make(map[string][]*webshareDirectEntry)
	for _, e := range entries {
		bySub[e.subnet24] = append(bySub[e.subnet24], e)
	}
	subKeys := make([]string, 0, len(bySub))
	for k := range bySub {
		subKeys = append(subKeys, k)
	}
	sort.Strings(subKeys)

	type subBucket struct {
		items   []*webshareDirectEntry
		pos     int
		weight  int
		current int
	}
	buckets := make([]*subBucket, 0, len(subKeys))
	for _, k := range subKeys {
		items := bySub[k]
		sort.Slice(items, func(i, j int) bool { return items[i].addr < items[j].addr })
		buckets = append(buckets, &subBucket{items: items, weight: len(items)})
	}

	out := make([]*webshareDirectEntry, 0, len(entries))
	for len(out) < len(entries) {
		bestIdx := -1
		activeWeight := 0
		for i, b := range buckets {
			if b.pos >= len(b.items) {
				continue
			}
			activeWeight += b.weight
			b.current += b.weight
			if bestIdx == -1 || b.current > buckets[bestIdx].current {
				bestIdx = i
			}
		}
		if bestIdx == -1 {
			break
		}
		chosen := buckets[bestIdx]
		chosen.current -= activeWeight
		out = append(out, chosen.items[chosen.pos])
		chosen.pos++
	}
	return out
}
