// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"net"
	"slices"
	"testing"
)

// overlayAddr is a synthetic tailnet address, built from octets so the fixture
// carries no address literal.
var overlayAddr = net.IPv4(100, 64, 0, 9).String()

// scanWith drives one supplemented scan against a fixed multicast result.
func scanWith(b *Browser, multicast map[string]Node) []Event {
	b.browseFunc = func(context.Context) map[string]Node { return multicast }
	return b.reconcileScan(b.scan(context.Background()))
}

// TestSupplementedNodeRidesTheSameStateMachine: a node only the supplement knows
// about is discovered, stays quiet while unchanged, and is evicted through the
// ordinary miss threshold once the supplement stops providing it.
func TestSupplementedNodeRidesTheSameStateMachine(t *testing.T) {
	supplying := true
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(3),
		WithSupplement(func(seen map[string]Node) map[string]Node {
			if supplying {
				seen["tail"] = node("tail")
			}
			return seen
		}))

	if got := eventsByType(scanWith(b, seenSet())); got[Discovered] != 1 {
		t.Fatalf("first supplied scan: got %v, want 1 discovered", got)
	}
	if evs := scanWith(b, seenSet()); len(evs) != 0 {
		t.Fatalf("unchanged supplied scan emitted %v", evs)
	}

	supplying = false
	for i := 0; i < 2; i++ {
		if evs := scanWith(b, seenSet()); len(evs) != 0 {
			t.Fatalf("miss %d emitted %v, want none before threshold", i+1, evs)
		}
	}
	if got := eventsByType(scanWith(b, seenSet())); got[Removed] != 1 {
		t.Fatalf("threshold miss: got %v, want the supplied node removed", got)
	}
}

// TestSupplementAnnotatingAMulticastNodeIsOneUpdate: adding an address to an
// mDNS-reported node is a single Updated, then quiet for as long as both agree.
func TestSupplementAnnotatingAMulticastNodeIsOneUpdate(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithSupplement(func(seen map[string]Node) map[string]Node {
		if n, ok := seen["a"]; ok {
			n.Addresses = append(slices.Clone(n.Addresses), overlayAddr)
			seen["a"] = n
		}
		return seen
	}))
	b.reconcile(seenSet(node("a"))) // known from a plain, unsupplemented reconcile

	if got := eventsByType(scanWith(b, seenSet(node("a")))); got[Updated] != 1 {
		t.Fatalf("first annotated scan: got %v, want 1 updated", got)
	}
	for i := 0; i < 3; i++ {
		if evs := scanWith(b, seenSet(node("a"))); len(evs) != 0 {
			t.Fatalf("annotated scan %d emitted %v, want none", i+1, evs)
		}
	}
	if got := b.Nodes()[0].Addresses; !slices.Contains(got, overlayAddr) {
		t.Fatalf("stored addresses %v lack the supplied one", got)
	}
}

// TestSupplementedNodesAreRekeyed: whatever key the supplement used, the node is
// stored under the browser's own key, so its mDNS record later lands on the same
// entry instead of beside it.
func TestSupplementedNodesAreRekeyed(t *testing.T) {
	byUUID := WithKeyFunc(func(n Node) string { return UUIDFromTXT(n.TXT) })
	viaTailnet := node("member-tail", "uuid=u-1")
	b := New("_nvpair-test._tcp", "local", byUUID, WithSupplement(func(seen map[string]Node) map[string]Node {
		seen["some-other-key"] = viaTailnet
		return seen
	}))

	scanWith(b, map[string]Node{})
	viaMDNS := node("member", "uuid=u-1")
	evs := scanWith(b, map[string]Node{"u-1": viaMDNS})
	if n := len(b.Nodes()); n != 1 {
		t.Fatalf("browser holds %d nodes, want the one member under one key", n)
	}
	for _, e := range evs {
		if e.Type == Discovered {
			t.Fatalf("the mDNS record was treated as a new node: %v", evs)
		}
	}
}

// TestEmptyMulticastIsStillExcusedWhenSupplied: the empty-scan grace protects the
// nodes only multicast knows about even while the supplement keeps reporting its
// own, and those supplied nodes are reconciled normally throughout.
func TestEmptyMulticastIsStillExcusedWhenSupplied(t *testing.T) {
	// A threshold shorter than the grace makes the grace observable: without it
	// the multicast-only nodes would be evicted inside the grace window.
	const threshold = 3
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(threshold), WithLivenessProbe(func(Node) bool { return false }),
		WithSupplement(func(seen map[string]Node) map[string]Node {
			seen["tail"] = node("tail")
			return seen
		}))
	scanWith(b, seenSet(node("a"), node("b"), node("c")))

	for i := 0; i < emptyScanGrace; i++ {
		for _, e := range scanWith(b, seenSet()) {
			if e.Type == Removed {
				t.Fatalf("empty multicast scan %d evicted %s inside the grace", i+1, e.Node.ID)
			}
		}
	}
	if n := len(b.Nodes()); n != 4 {
		t.Fatalf("browser holds %d nodes after the grace, want 4", n)
	}

	removed := 0
	for i := 0; i < threshold+1; i++ {
		removed += eventsByType(scanWith(b, seenSet()))[Removed]
	}
	if removed != 3 {
		t.Fatalf("removed %d after the grace, want the three multicast-only nodes", removed)
	}
	if got := b.Nodes(); len(got) != 1 || got[0].ID != "tail" {
		t.Fatalf("remaining %v, want only the supplied node", got)
	}
}
