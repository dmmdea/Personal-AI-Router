// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"slices"

	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

// defaultEngineManagerPort is the fixed em port (nvpair-ui-broker
// engineManagerHTTPPort). The locator proves membership against it.
const defaultEngineManagerPort = 14322

// defaultClusterPorts are the fixed ports the broker registers for a node's
// cluster-facing services (nvpair-ui-broker nodeInfoHTTPPort, errors, workload,
// cluster-manager, engineManagerHTTPPort, engineControlPort). A member located
// over the tailnet before this process ever heard its mDNS record is described
// with them. The inference proxies (ol/lm) are left out: their ports are picked
// at runtime, so a record that has never been heard cannot name them.
var defaultClusterPorts = map[noderec.ServiceKey]int{
	noderec.ServiceNodeInfo:      14318,
	noderec.ServiceErrors:        14319,
	noderec.ServiceWorkload:      14320,
	noderec.ServiceCluster:       14321,
	noderec.ServiceEngineManager: defaultEngineManagerPort,
	noderec.ServiceEngineControl: 14323,
}

// tailnetSourceTXT tags a record the merge built from the tailnet locator rather
// than heard over mDNS. It never leaves this process: the directory carries
// addresses and services, not TXT. noderec.ParseTXT ignores the key, and the
// liveness probe reads it (see daemon.reachable) so an overlay record lives and
// dies by the pinned proof that created it.
const tailnetSourceTXT = "src=tailnet"

// isOverlayRecord reports whether n was built by the merge from a locator proof.
func isOverlayRecord(n RawNode) bool { return slices.Contains(n.TXT, tailnetSourceTXT) }

// tailnetCarryScans is how many consecutive scans a located member's mDNS record
// may be missing before its tailnet-only record replaces it. A single dropped
// multicast answer is routine on a busy LAN; flipping the record on every one
// would publish an update storm of address changes to every consumer of the
// directory. The value matches the browser's probeAfterMisses, the point at which
// the browser itself starts treating an absence as worth checking.
const tailnetCarryScans = 3

// tailnetMerge folds located members into each scan's mDNS results (the
// browser's WithSupplement hook). It runs on the browser's scan goroutine only,
// so its own state needs no lock.
//
// For a member the locator proved at an overlay address:
//
//   - heard over mDNS: the record stands as the member published it, with the
//     overlay address added so the liveness probe can still reach it after the
//     LAN goes away;
//   - not heard for fewer than tailnetCarryScans scans, having just been heard:
//     the last mDNS record is carried forward unchanged, so a dropped multicast
//     answer is no event at all;
//   - not heard for longer, or never heard: a record describing the member at
//     its overlay address only, under the instance name of its last mDNS record
//     (or its Tailscale hostname — the same OS hostname PAIR uses as the instance
//     name — if it was never heard). Its identity is the UUID the pinned
//     handshake proved, and its ports are the fixed cluster ports: an mDNS
//     record is unauthenticated, so nothing in it decides where this node dials
//     a proven member, and the inference proxies (ol/lm), which the broker
//     advertises only while the engine behind them is healthy, are left out.
//
// A member that is neither heard nor located is left out, and the browser's
// ordinary miss threshold and liveness probe decide its fate as before.
type tailnetMerge struct {
	locate func() map[string]tailnetHint

	lastHeard map[string]RawNode // member UUID -> its last mDNS record
	// silent counts the located scans a member has gone unheard since mDNS last
	// heard it. Only hearing the member resets it: a member that drops out of
	// the locator for a refresh (one failed proof) keeps its count and comes back
	// on its overlay record directly, instead of being carried through its stale
	// LAN record again, which would publish two address flips for nothing.
	silent map[string]int
}

func newTailnetMerge(locate func() map[string]tailnetHint) *tailnetMerge {
	return &tailnetMerge{
		locate:    locate,
		lastHeard: make(map[string]RawNode),
		silent:    make(map[string]int),
	}
}

// supplement is the WithSupplement hook. It runs on the browser's scan goroutine,
// the heart of the only worker the broker cannot do without, so a fault in it
// costs one scan's supplement and is logged, never the process.
func (m *tailnetMerge) supplement(seen map[string]RawNode) (out map[string]RawNode) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("tailnet merge failed; this scan carries mDNS sightings only", "panic", r)
			out = seen
		}
	}()
	hints := m.locate()
	for k, n := range seen {
		uuid := UUIDFromTXT(n.TXT)
		if uuid == "" {
			continue
		}
		m.lastHeard[uuid] = n
		delete(m.silent, uuid)
		if h, ok := hints[uuid]; ok {
			seen[k] = withAddress(n, h.ip)
		}
	}
	for uuid, h := range hints {
		// Heard means a record that names this member. The browser falls back to
		// the instance name for a record without uuid=, so a record that merely
		// sits on the member's key must not stand in for the member.
		if n, onKey := seen[uuid]; onKey && UUIDFromTXT(n.TXT) == uuid {
			continue
		}
		m.silent[uuid]++
		if last, ok := m.lastHeard[uuid]; ok && m.silent[uuid] < tailnetCarryScans {
			seen[uuid] = withAddress(last, h.ip)
			continue
		}
		seen[uuid] = m.overlayRecord(uuid, h)
	}
	return seen
}

// overlayRecord describes a member at its overlay address alone. Dialing the
// LAN addresses from its last mDNS record would mean sending traffic to
// whichever machine holds those addresses on the network this node is on now.
func (m *tailnetMerge) overlayRecord(uuid string, h tailnetHint) RawNode {
	name := h.hostName
	if last, ok := m.lastHeard[uuid]; ok && last.ID != "" {
		name = last.ID
	}
	if name == "" {
		name = h.ip
	}
	// The cluster principal of a member is its node UUID, the one the pinned
	// certificate carries.
	rec := noderec.NodeRecord{HostUUID: uuid, ClusterUUID: uuid, IP: h.ip, Services: defaultClusterPorts}
	return RawNode{
		ID:        name,
		Port:      noderec.SRVPort,
		Addresses: []string{h.ip},
		TXT:       append(rec.TXT(), tailnetSourceTXT),
	}
}

// withAddress returns n with ip added to its resolved addresses (a copy; the
// browser's stored records are never aliased).
func withAddress(n RawNode, ip string) RawNode {
	if slices.Contains(n.Addresses, ip) {
		return n
	}
	n.Addresses = append(slices.Clone(n.Addresses), ip)
	return n
}

// tailnetCandidates is netpick.Candidates for a member with a proven overlay
// address: the member's own ranking, in its own order, with the overlay address
// in the last place inside the cap.
//
// The placement matters. Candidates caps a node's list at
// noderec.MaxAdvertisedIPs and ranks 100.64/10 last, so a member with a few
// LAN, Docker or VM addresses would have its overlay address cut — here and
// again in every process that reads the directory. Taking the last slot within
// the cap (displacing the member's lowest-ranked address only when the list is
// full) keeps it everywhere without outranking any address the member ranked
// from its own evidence: every dialer prefers the best-ranked address that
// answers, so at home the LAN still wins, and away the overlay address is the
// one that answers.
func tailnetCandidates(txt, addrs, overlay []string) []string {
	base := netpick.Candidates(txt, addrs)
	if len(overlay) == 0 {
		return base
	}
	var tail []string
	for _, a := range overlay {
		if a != "" && !slices.Contains(tail, a) && len(tail) < noderec.MaxAdvertisedIPs {
			tail = append(tail, a)
		}
	}
	room := noderec.MaxAdvertisedIPs - len(tail)
	out := make([]string, 0, noderec.MaxAdvertisedIPs)
	for _, a := range base {
		if len(out) >= room {
			break
		}
		if !slices.Contains(tail, a) {
			out = append(out, a)
		}
	}
	return append(out, tail...)
}
