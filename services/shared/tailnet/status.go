// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tailnet reads the local Tailscale client's view of its tailnet: which
// peers exist, whether each is online, and the overlay addresses it can be reached
// at. It answers one question for PAIR's discovery — "where else might a cluster
// peer be listening?" — when multicast DNS cannot cross the link between two
// members (Tailscale carries no multicast, so mDNS never sees a member that is only
// reachable over the tailnet).
//
// Nothing here is trust. A peer list names candidate addresses and nothing more:
// the caller must prove who answers at an address (the scanner does it with the
// cluster's pinned certificates) before treating it as a member. Hostnames,
// DNS names and online flags are hints for display and scheduling only.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
)

// Peer is one tailnet device as the local client reports it.
type Peer struct {
	// HostName is the device's OS hostname, as tailscaled reports it. PAIR names
	// a node by its OS hostname too, so this is the same string the node uses as
	// its mDNS instance name.
	HostName string
	// DNSName is the MagicDNS name with its trailing dot, e.g. "node-a.example.ts.net.".
	DNSName string
	// IPv4 holds the peer's overlay IPv4 addresses (the 100.64/10 range), in the order
	// the client listed them. IPv6 overlay addresses are dropped: PAIR publishes
	// and dials IPv4 only (engine-manager binds IPv4 only).
	IPv4 []string
	// Online is the coordination server's view of whether the device is connected
	// to the control plane — not proof that it is reachable, nor its absence
	// proof that it is not.
	Online bool
	// Expired reports that the device's node key has expired: it cannot carry
	// traffic until it is re-authenticated.
	Expired bool
	// SharedIn reports a device another tailnet shared into this one (Tailscale
	// node sharing). It belongs to someone else's network and is never a member
	// of a cluster formed on this one.
	SharedIn bool
}

// Snapshot is the parsed result of one status read.
type Snapshot struct {
	// Running reports BackendState == "Running". Any other state (Stopped,
	// NeedsLogin, Starting, NoState) means the tailnet is not usable right now
	// and Peers is empty.
	Running bool
	// SelfIPv4 holds this device's own overlay IPv4 addresses.
	SelfIPv4 []string
	// Peers is every peer in the network map, sorted by HostName then first
	// address so callers iterate deterministically.
	Peers []Peer
}

// Runner returns the raw `status --json` output of the local Tailscale client.
type Runner func(ctx context.Context) ([]byte, error)

// ErrUnavailable reports that no Tailscale client could be found on this host.
// It is the normal answer on a machine without Tailscale and should be logged
// once, not treated as a fault.
var ErrUnavailable = errors.New("tailscale client not found")

// Status reads and parses the local client's status through run.
func Status(ctx context.Context, run Runner) (Snapshot, error) {
	if run == nil {
		return Snapshot{}, ErrUnavailable
	}
	out, err := run(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Parse(out)
}

// statusJSON is the subset of `tailscale status --json` this package reads.
// The CLI documents the format as subject to change, so every field is optional
// and unknown fields are ignored.
type statusJSON struct {
	BackendState string              `json:"BackendState"`
	Self         *peerJSON           `json:"Self"`
	Peer         map[string]peerJSON `json:"Peer"`
}

// peerJSON reads only routing facts. Fields that identify people or networks
// (User, Addrs, CurrentTailnet, AuthURL) are never decoded, so they cannot end
// up in memory or in a log line.
type peerJSON struct {
	HostName        string   `json:"HostName"`
	DNSName         string   `json:"DNSName"`
	TailscaleIPs    []string `json:"TailscaleIPs"`
	Online          bool     `json:"Online"`
	Expired         bool     `json:"Expired"`
	AltSharerUserID int64    `json:"AltSharerUserID"`
}

// Parse decodes a `tailscale status --json` document. A backend that is not
// Running yields Running=false and no peers, without an error: that is a state,
// not a malformed document.
func Parse(data []byte) (Snapshot, error) {
	var raw statusJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("parse tailscale status: %w", err)
	}
	snap := Snapshot{Running: raw.BackendState == "Running"}
	if !snap.Running {
		return snap, nil
	}
	if raw.Self != nil {
		snap.SelfIPv4 = overlayIPv4(raw.Self.TailscaleIPs)
	}
	for _, p := range raw.Peer {
		snap.Peers = append(snap.Peers, Peer{
			HostName: p.HostName,
			DNSName:  p.DNSName,
			IPv4:     overlayIPv4(p.TailscaleIPs),
			Online:   p.Online,
			Expired:  p.Expired,
			SharedIn: p.AltSharerUserID != 0,
		})
	}
	sort.Slice(snap.Peers, func(i, j int) bool {
		a, b := snap.Peers[i], snap.Peers[j]
		if a.HostName != b.HostName {
			return a.HostName < b.HostName
		}
		return firstOf(a.IPv4) < firstOf(b.IPv4)
	})
	return snap, nil
}

// cgnat is 100.64/10, the range Tailscale assigns overlay IPv4 addresses from.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// IsOverlayIPv4 reports whether s is an IPv4 literal inside 100.64/10.
//
// The range is shared with carrier-grade NAT, so membership alone never proves an
// address is a tailnet one; it only filters what a status read may hand back.
func IsOverlayIPv4(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	return ip4 != nil && cgnat.Contains(ip4)
}

func overlayIPv4(ips []string) []string {
	var out []string
	for _, s := range ips {
		if IsOverlayIPv4(s) {
			out = append(out, net.ParseIP(s).To4().String())
		}
	}
	return out
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
