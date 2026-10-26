package cubevs

import "testing"

// ------- v3 custom-port encoding (buildV3Entries) -------------------

func TestExpandDefaultPortSet(t *testing.T) {
	// The default (host, port) set used when an L7 rule omits port.
	ports := expandDefaultPortSet()
	if got, want := len(ports), 2; got != want {
		t.Fatalf("len(expandDefaultPortSet())=%d, want %d", got, want)
	}
	want := map[uint16]uint8{
		htonsPort(80):  L7SchemeHTTP,
		htonsPort(443): L7SchemeHTTPS,
	}
	for _, p := range ports {
		scheme, ok := want[p.Port]
		if !ok {
			t.Fatalf("unexpected default port 0x%04x", p.Port)
		}
		if p.Scheme != scheme {
			t.Fatalf("port 0x%04x scheme=%d, want %d", p.Port, p.Scheme, scheme)
		}
	}
}

func TestBuildV3EntriesL7ExplicitPorts(t *testing.T) {
	// L7 with explicit (port, scheme) tuples -> one /48 entry per tuple,
	// each carrying scheme + expiry, ip in network byte order.
	const ip uint32 = 0x01020304
	const expires uint64 = 1234
	entries := buildV3Entries(lpmKey{Prefixlen: 32, IP: ip}, netPolicyFlagL7Required,
		[]l7PortEntry{
			{Port: htonsPort(8080), Scheme: L7SchemeHTTP},
			{Port: htonsPort(8443), Scheme: L7SchemeHTTPS},
		}, expires)
	if got, want := len(entries), 2; got != want {
		t.Fatalf("len(entries)=%d, want %d", got, want)
	}
	got := map[uint16]uint8{}
	for _, e := range entries {
		if e.key.Prefixlen != 48 {
			t.Fatalf("key.Prefixlen=%d, want 48 (exact ip+port)", e.key.Prefixlen)
		}
		if e.key.IP != ip {
			t.Fatalf("key.IP=0x%08x, want 0x%08x", e.key.IP, ip)
		}
		if e.value.Flags != netPolicyFlagL7Required {
			t.Fatalf("value.Flags=%d, want %d", e.value.Flags, netPolicyFlagL7Required)
		}
		if e.value.ExpiresAtNS != expires {
			t.Fatalf("value.ExpiresAtNS=%d, want %d", e.value.ExpiresAtNS, expires)
		}
		got[e.key.Port] = e.value.Scheme
	}
	if got[htonsPort(8080)] != L7SchemeHTTP {
		t.Fatalf("port 8080 scheme=%d, want HTTP", got[htonsPort(8080)])
	}
	if got[htonsPort(8443)] != L7SchemeHTTPS {
		t.Fatalf("port 8443 scheme=%d, want HTTPS", got[htonsPort(8443)])
	}
}

func TestBuildV3EntriesL7DefaultExpansion(t *testing.T) {
	// L7 with no port set -> default {80/http, 443/https} expansion,
	// both as /48 exact entries.
	entries := buildV3Entries(lpmKey{Prefixlen: 32, IP: 0x01020304}, netPolicyFlagL7Required, nil, 0)
	if got, want := len(entries), 2; got != want {
		t.Fatalf("len(entries)=%d, want 2 (default {80,443})", got)
	}
	for _, e := range entries {
		if e.key.Prefixlen != 48 {
			t.Fatalf("key.Prefixlen=%d, want 48", e.key.Prefixlen)
		}
	}
}

func TestBuildV3EntriesPlainAllow(t *testing.T) {
	// Non-L7 allow -> single ip-only /32 entry, port=0, scheme=NONE.
	entries := buildV3Entries(lpmKey{Prefixlen: 32, IP: 0x01020304}, 0, nil, 0)
	if got, want := len(entries), 1; got != want {
		t.Fatalf("len(entries)=%d, want 1", got)
	}
	e := entries[0]
	if e.key.Prefixlen != 32 {
		t.Fatalf("key.Prefixlen=%d, want 32 (ip-only)", e.key.Prefixlen)
	}
	if e.key.Port != 0 {
		t.Fatalf("key.Port=%d, want 0 (non-L7)", e.key.Port)
	}
	if e.value.Scheme != L7SchemeNone {
		t.Fatalf("value.Scheme=%d, want NONE", e.value.Scheme)
	}
}

func TestBuildV3EntriesSubnet(t *testing.T) {
	// Non-L7 subnet -> single entry at the source prefixlen (< 32).
	entries := buildV3Entries(lpmKey{Prefixlen: 24, IP: 0x01020304}, 0, nil, 0)
	if got, want := len(entries), 1; got != want {
		t.Fatalf("len(entries)=%d, want 1", got)
	}
	if entries[0].key.Prefixlen != 24 {
		t.Fatalf("key.Prefixlen=%d, want 24 (subnet)", entries[0].key.Prefixlen)
	}
	if entries[0].key.Port != 0 {
		t.Fatalf("key.Port=%d, want 0", entries[0].key.Port)
	}
}

func TestBuildV3EntriesExpiryCopied(t *testing.T) {
	// Expiry is carried verbatim into every expanded entry.
	entries := buildV3Entries(lpmKey{Prefixlen: 32, IP: 1}, netPolicyFlagL7Required,
		[]l7PortEntry{{Port: htonsPort(8080), Scheme: L7SchemeHTTP}}, 999)
	for _, e := range entries {
		if e.value.ExpiresAtNS != 999 {
			t.Fatalf("value.ExpiresAtNS=%d, want 999", e.value.ExpiresAtNS)
		}
	}
}

func TestLpmKeyV3NetworkByteOrder(t *testing.T) {
	// A custom port must be stored in network byte order in the LPM key,
	// so the datapath (which compares against tcphdr->dest) matches.
	const port uint16 = 0x1234
	entries := buildV3Entries(lpmKey{IP: 1}, netPolicyFlagL7Required,
		[]l7PortEntry{{Port: htonsPort(port), Scheme: L7SchemeHTTP}}, 0)
	if len(entries) != 1 {
		t.Fatalf("len(entries)=%d, want 1", len(entries))
	}
	if got := entries[0].key.Port; got != htonsPort(port) {
		t.Fatalf("key.Port=0x%04x, want network byte order 0x%04x", got, htonsPort(port))
	}
}
