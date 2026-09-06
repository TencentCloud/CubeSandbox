// Per-sandbox domain rule index used by the user-space DNS learner.
//
// The BPF side keeps a dns_allow_v2 LPM trie keyed on reversed lowercase
// domain names (see src/cubevs.h dns_allow_key). Its purpose is to gate DNS
// queries at egress: a match tracks the query so its response can be learned.
//
// DNS learning happens in user space, so the *learner* also needs to consult
// the same rule set to decide whether to learn an A record. Rather than open
// the BPF map on every DNS event (expensive) or duplicate the LPM trie in Go
// (nontrivial for O(hundreds) of rules), we build a small in-process index
// directly from the plan's dns_allow_v2 rules and query it with an exact-match
// hash plus a longest-suffix scan.
//
// The index is rebuilt from scratch on every applyUpdate, so it always
// reflects the last policy the manager applied. It is persisted to the JSON
// snapshot (as PolicySnapshot.DomainRules) so a Cubelet restart can rebuild it
// from disk — the BPF dns_allow_v2 map survives restarts, and persisting the
// rules keeps DNS learning working until the next control-plane push.

package cubevs

import (
	"fmt"
	"sort"
	"strings"
)

// domainRule captures one dns_allow_v2 rule in the form the learner consumes.
// flags and ports are what a matched query would have inherited into
// dns_query_track, and are exactly what learnedAllowOutRows needs to derive
// the allow_out_v3 rows for a resolved IP.
type domainRule struct {
	// pattern is the user-facing domain, lower-cased and without a trailing
	// dot. Wildcard rules keep the leading "*.".
	pattern string
	kind    domainRuleKind
	// suffix is the wildcard base: "example.com" for "*.example.com". Empty
	// for exact rules.
	suffix string
	flags  uint8
	// ports carries network-order (port, scheme) tuples. Empty means the
	// datapath default set {80/http, 443/https}.
	ports []l7PortEntry
}

type domainRuleKind uint8

const (
	domainRuleExact    domainRuleKind = iota // "example.com"
	domainRuleWildcard                       // "*.example.com"
)

// domainRuleSet is the queryable form. Exact rules go into a hash so the
// common case (a fully-qualified QName) is O(1). Wildcard rules go into a
// slice sorted by suffix length descending so a linear scan finds the longest
// match first.
//
// There is deliberately no catch-all bucket: a bare "*" is rejected by
// isDNSAllowTarget and makeDNSAllowRule, so it can never reach this index.
type domainRuleSet struct {
	exact     map[string]*domainRule
	wildcards []*domainRule // sorted: longest suffix first
}

// normalizeQName applies the same normalisation makeDNSAllowRule uses on rule
// domains, so a QName from the wire and a rule pattern compare equal.
func normalizeQName(qname string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(qname), "."))
}

// domainRulesFromPlan converts plan rules into the JSON-serialisable form
// stored in the snapshot.
//
// applyUpdate deliberately goes through this on its way to the query index
// (persisted form first, then buildDomainRuleSetFromPersisted on the result)
// rather than deriving the two in parallel from the plan. Both then come from
// one source, so the index a restart rebuilds from disk is identical to the one
// the live process is using.
func domainRulesFromPlan(rules []dnsAllowRule) []DomainRule {
	out := make([]DomainRule, 0, len(rules))
	for i := range rules {
		r := &rules[i]
		ports := make([]L7Port, 0, r.value.PortCount)
		for j := uint8(0); j < r.value.PortCount && j < maxL7PortsPerHost; j++ {
			ports = append(ports, L7Port{
				Port:   ntohsPort(r.value.Ports[j].Port),
				Scheme: r.value.Ports[j].Scheme,
			})
		}
		out = append(out, DomainRule{
			Domain: r.domain,
			Flags:  r.value.Flags,
			Ports:  ports,
		})
	}
	return out
}

// buildDomainRuleSetFromPersisted rebuilds the index from the persisted rule
// list. Called from applyUpdate and when a snapshot is loaded after a restart.
// Malformed entries are skipped rather than failing the whole set:
// they were already validated upstream by buildNetPolicyPlan, so one bad row
// means a hand-edited snapshot, and keeping learning alive for the rest of the
// rules beats refusing to learn at all.
func buildDomainRuleSetFromPersisted(rules []DomainRule) *domainRuleSet {
	rs := &domainRuleSet{exact: make(map[string]*domainRule, len(rules))}
	for _, r := range rules {
		rule := parseDomainRule(r)
		if rule == nil {
			continue
		}
		switch rule.kind {
		case domainRuleExact:
			rs.exact[rule.pattern] = rule
		case domainRuleWildcard:
			rs.wildcards = append(rs.wildcards, rule)
		}
	}
	// Longest suffix first so lookup can return on the first suffix match.
	sort.SliceStable(rs.wildcards, func(i, j int) bool {
		return len(rs.wildcards[i].suffix) > len(rs.wildcards[j].suffix)
	})
	return rs
}

// buildPlainDomainRuleSet builds a lookup index over raw plain (non-L7)
// allow_out domain strings. Used by markL3AllowedByPlainCover so plain-cover
// detection and learn-time matching share one implementation of the exact /
// "*.base" semantics.
func buildPlainDomainRuleSet(domains []string) *domainRuleSet {
	rules := make([]DomainRule, 0, len(domains))
	for _, d := range domains {
		rules = append(rules, DomainRule{Domain: d})
	}
	return buildDomainRuleSetFromPersisted(rules)
}

// parseDomainRule mirrors makeDNSAllowRule's classification and validation but
// produces the in-memory shape. Returns nil for anything makeDNSAllowRule
// would have rejected, so the index can never hold a pattern the datapath does
// not have a key for.
func parseDomainRule(r DomainRule) *domainRule {
	pat := normalizeQName(r.Domain)
	if pat == "" {
		return nil
	}
	ports := make([]l7PortEntry, 0, len(r.Ports))
	for _, p := range r.Ports {
		ports = append(ports, l7PortEntry{Port: htonsPort(p.Port), Scheme: p.Scheme})
	}
	if len(ports) > maxL7PortsPerHost {
		return nil
	}

	base := pat
	kind := domainRuleExact
	suffix := ""
	if strings.Contains(pat, "*") {
		// Only a single leading "*." is representable as a dns_allow_v2 key.
		if strings.Count(pat, "*") != 1 || !strings.HasPrefix(pat, "*.") || len(pat) <= len("*.") {
			return nil
		}
		base = pat[2:]
		kind = domainRuleWildcard
		suffix = base
	}
	if base == "" || len(base) >= maxDNSNameLen-1 || !isValidDNSDomainName(base) {
		return nil
	}
	return &domainRule{
		pattern: pat,
		kind:    kind,
		suffix:  suffix,
		flags:   r.Flags,
		ports:   ports,
	}
}

// lookup returns the rule the BPF LPM trie would match for qname, or nil.
//
// Precedence:
//
//  1. Exact match on the full QName.
//  2. Longest wildcard suffix. "*.example.com" matches "a.example.com" and
//     "a.b.example.com" but *not* the apex "example.com" — the apex needs its
//     own exact rule.
//
// This is equivalent to the datapath's longest-prefix match on the reversed
// name. The query key is reverse(qname) + '\0' with prefixlen
// (len(qname)+1)*8; an exact rule's key is that same byte string, while any
// wildcard matching the same QName has a strictly shorter key (its suffix is
// shorter than the QName and its terminator is '.'). So an exact hit is always
// the longest prefix, and among wildcards the longest suffix wins.
func (rs *domainRuleSet) lookup(qname string) *domainRule {
	if rs == nil {
		return nil
	}
	qname = normalizeQName(qname)
	if qname == "" {
		return nil
	}
	if r, ok := rs.exact[qname]; ok {
		return r
	}
	for _, r := range rs.wildcards {
		// Require the leading dot so "aexample.com" does not match a rule for
		// "*.example.com".
		if strings.HasSuffix(qname, "."+r.suffix) {
			return r
		}
	}
	return nil
}

// decodeDNSAllowKey reverses makeDNSAllowRule's encoding: it recovers the
// user-facing pattern from a dns_allow_v2 (key, value) pair.
//
// The encoding is lossless — value.NameLen gives the used length, the last
// byte distinguishes exact ('\0') from wildcard ('.'), and the preceding bytes
// are the reversed domain. reconcile uses this to compare the live map against
// the persisted mirror.
func decodeDNSAllowKey(key dnsAllowKey, value dnsAllowValue) (string, error) {
	n := int(value.NameLen)
	if n < 2 || n > maxDNSNameLen {
		return "", fmt.Errorf("invalid dns_allow_v2 name_len: %d", n) //nolint:err113
	}
	reversed := key.Name[:n-1]
	domain := make([]byte, len(reversed))
	for i := range reversed {
		domain[i] = reversed[len(reversed)-1-i]
	}
	switch key.Name[n-1] {
	case 0:
		return string(domain), nil
	case '.':
		return "*." + string(domain), nil
	default:
		return "", fmt.Errorf("invalid dns_allow_v2 terminator: %#x", key.Name[n-1]) //nolint:err113
	}
}
