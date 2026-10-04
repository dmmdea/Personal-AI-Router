// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"nvpair-shared/tailnet"
)

// TestOverlayRecordIsKeptWhileTheMemberServesInference: a member saturated by
// inference cannot finish a handshake, but the bytes the local proxies receive
// from its engine prove it is alive; its overlay record must survive that.
func TestOverlayRecordIsKeptWhileTheMemberServesInference(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	stale := now.Add(-10 * time.Minute)
	d := &daemon{tailnet: vouchingLocator(uuidMember, overlayIP, stale, &now), lastActivityAt: map[string]time.Time{}}
	rec := newTailnetMerge(located).overlayRecord(uuidMember, tailnetHint{uuid: uuidMember, ip: overlayIP, hostName: "m"})

	if d.reachable(rec) {
		t.Fatal("precondition: a stale proof alone keeps the record")
	}
	d.lastActivityAt[uuidMember] = time.Now()
	if !d.reachable(rec) {
		t.Fatal("a member streaming inference lost its overlay record for missing a handshake")
	}
}

// TestAProbeFaultIsCountedNotFatal: a probe parses TLS from any tailnet device;
// a panic there is one failed probe and counts toward switching the locator off,
// it never takes the scanner down.
func TestAProbeFaultIsCountedNotFatal(t *testing.T) {
	mesh, _, _, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.l.dial = func(context.Context, string, string) (net.Conn, error) { panic("boom in a probe") }
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "x", IPv4: []string{testOverlay(9)}, Online: true}}}

	_, faulted := f.l.safeRefresh(context.Background())
	if !faulted {
		t.Fatal("a panicking probe was not counted as a fault")
	}
}

// TestANetworkChangeKeepsDefiniteRejections: after a network change only the
// devices that did not answer are asked again; one that answered with a
// certificate that is not a member's stays parked until the pins change.
func TestANetworkChangeKeepsDefiniteRejections(t *testing.T) {
	mesh, _, _, outsiderDir := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(3)] = startTLSPeer(t, outsiderDir).addr // definitive: unpinned certificate
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{
		{HostName: "outsider", IPv4: []string{testOverlay(3)}, Online: true},
		{HostName: "silent", IPv4: []string{testOverlay(9)}, Online: true}, // refused: transient
	}}
	f.l.refresh(context.Background())

	f.l.poke()
	f.l.applyKick()
	f.l.refresh(context.Background())
	if n := f.dialCount(testOverlay(3)); n != 1 {
		t.Fatalf("a network change re-dialed a definite non-member (%d dials)", n)
	}
	if n := f.dialCount(testOverlay(9)); n != 2 {
		t.Fatalf("a network change did not re-ask a device that never answered (%d dials)", n)
	}

	f.l.pokeTrust()
	f.l.applyKick()
	f.l.refresh(context.Background())
	if n := f.dialCount(testOverlay(3)); n != 2 {
		t.Fatalf("a pin change did not lift the definite park (%d dials)", n)
	}
}

// TestMembersArePublishedBeforeStrangersAreProbed: a slow sweep of devices that
// are not members must not hold a known member back.
func TestMembersArePublishedBeforeStrangersAreProbed(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background()) // the member is now a recent member

	release := make(chan struct{})
	member := f.l.dial
	f.l.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, _ := net.SplitHostPort(addr); host == testOverlay(9) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("no answer")
		}
		return member(ctx, network, addr)
	}
	f.snap.Peers = append(f.snap.Peers, tailnet.Peer{HostName: "stranger", IPv4: []string{testOverlay(9)}, Online: true})
	f.l.replaceHints(nil)

	done := make(chan struct{})
	go func() { f.l.refresh(context.Background()); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := f.l.current()[uuidMember]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the member was not published while a stranger was still being probed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("precondition: the refresh finished before the stranger was released")
	default:
	}
	close(release)
	<-done
}

// TestAProofIsStampedWhenItCompletes: a long refresh must not record proofs that
// are already old, or the vouch window shrinks by the refresh's length.
func TestAProofIsStampedWhenItCompletes(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	addr := startTLSPeer(t, memberDir).addr
	f.targets[testOverlay(2)] = addr
	start := f.now
	f.l.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		f.advance(45 * time.Second) // the handshake happens late in a long refresh
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background())

	f.l.mu.Lock()
	at := f.l.verified[testOverlay(2)].at
	f.l.mu.Unlock()
	if !at.Equal(start.Add(45 * time.Second)) {
		t.Fatalf("proof stamped at %v, want the completion time %v", at, start.Add(45*time.Second))
	}
}

// TestAMemberSilentPastTheWindowBacksOff: a member that stopped answering long
// ago is not re-dialed on every refresh forever; past the member window it backs
// off like any other device.
func TestAMemberSilentPastTheWindowBacksOff(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background())

	f.mu.Lock()
	f.targets[testOverlay(2)] = ""
	f.mu.Unlock()
	f.advance(tailnetMemberWindow + time.Second)
	if retry := f.l.refresh(context.Background()); retry {
		t.Fatal("a member silent past the window still asks for quick retries")
	}
	before := f.dialCount(testOverlay(2))
	f.l.refresh(context.Background())
	if n := f.dialCount(testOverlay(2)); n != before {
		t.Fatalf("a member silent past the window was re-dialed inside its backoff (%d -> %d)", before, n)
	}
}
