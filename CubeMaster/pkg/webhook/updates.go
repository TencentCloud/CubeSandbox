// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"net"
	"strings"
)

// NetworkFields holds only the requested L3/DNS projection, never L7 rules.
type NetworkFields struct {
	AllowOut, DenyOut []string
	DenyInternet      bool
}

func validateNetwork(n *NetworkFields) error {
	if n == nil {
		return nil
	}
	if len(n.AllowOut)+len(n.DenyOut) > 256 {
		return ErrCaptureTooLarge
	}
	total := 0
	for _, list := range [][]string{n.AllowOut, n.DenyOut} {
		for _, v := range list {
			if len(v) > 1024 {
				return ErrCaptureTooLarge
			}
			total += len(v)
			if total > 16*1024 {
				return ErrCaptureTooLarge
			}
		}
	}
	return nil
}
func cloneNetwork(n *NetworkFields) *NetworkFields {
	if n == nil {
		return nil
	}
	out := &NetworkFields{DenyInternet: n.DenyInternet}
	clone := func(in []string) []string {
		if in == nil {
			return nil
		}
		a := make([]string, len(in))
		for i, v := range in {
			a[i] = strings.Clone(v)
		}
		return a
	}
	out.AllowOut = clone(n.AllowOut)
	out.DenyOut = clone(n.DenyOut)
	return out
}
func networkCIDR(raw string) string {
	s := strings.TrimSpace(raw)
	if ip, n, err := net.ParseCIDR(s); err == nil && ip.To4() != nil {
		return n.String()
	}
	if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
		return ip.To4().String() + "/32"
	}
	return ""
}
func mapNetwork(n *NetworkFields) *NetworkEgressData {
	if n == nil {
		return nil
	}
	// Empty repeated proto fields arrive as nil. Match the producer's nil
	// arrays, including categories with no entries after splitting AllowOut.
	out := &NetworkEgressData{}
	for _, raw := range n.AllowOut {
		if cidr := networkCIDR(raw); cidr != "" {
			out.AllowedCIDRs = append(out.AllowedCIDRs, cidr)
		} else {
			s := strings.TrimSpace(raw)
			// Node success has already validated domain syntax. Do not reinterpret
			// unsupported IP/CIDR inputs as domains.
			if s != "" && !strings.ContainsAny(s, "/:") && net.ParseIP(s) == nil {
				out.AllowedDomains = append(out.AllowedDomains, strings.ToLower(strings.TrimSuffix(s, ".")))
			}
		}
	}
	if n.DenyInternet {
		out.DeniedCIDRs = []string{"0.0.0.0/0"}
	} else {
		for _, v := range n.DenyOut {
			if c := networkCIDR(v); c != "" {
				out.DeniedCIDRs = append(out.DeniedCIDRs, c)
			}
		}
	}
	return out
}
