// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/tailnet"
)

const (
	uuidSelf    = "11111111-1111-4111-8111-111111111111"
	uuidMember  = "22222222-2222-4222-8222-222222222222"
	uuidOutside = "33333333-3333-4333-8333-333333333333"
)

// pinCert pins the certificate in peerDir/node.crt into clusterDir/trusted, the
// on-disk shape nvpair-cluster-manager writes after a pairing completes.
func pinCert(t *testing.T, clusterDir, peerDir, peerUUID string) {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(peerDir, "node.crt"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"nodeUuid": peerUUID, "certPem": string(certPEM)})
	if err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(clusterDir, "trusted")
	if err := os.MkdirAll(trusted, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trusted, peerUUID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// tlsPeer is an engine-manager stand-in: it completes mTLS handshakes with the
// keypair in dir and records whether the client ever presented a certificate.
type tlsPeer struct {
	addr          string
	handshakes    atomic.Int32
	attempts      atomic.Int32 // handshakes finished, successful or not
	sawClientCert atomic.Bool
}

// waitAttempt blocks until the server has finished at least one handshake
// attempt, so an assertion about what it saw is never vacuous.
func (p *tlsPeer) waitAttempt(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.attempts.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the server never finished a handshake attempt")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startTLSPeer(t *testing.T, dir string) *tlsPeer {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	p := &tlsPeer{}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		// Request rather than require, so the test can observe that a client
		// rejecting this server never sends its leaf at all.
		ClientAuth: tls.RequestClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tc := c.(*tls.Conn)
				_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
				if err := tc.Handshake(); err == nil {
					p.handshakes.Add(1)
				}
				if len(tc.ConnectionState().PeerCertificates) > 0 {
					p.sawClientCert.Store(true)
				}
				p.attempts.Add(1)
			}(c)
		}
	}()
	return p
}

// locatorFixture is a locator for uuidSelf whose dialer maps overlay addresses
// to local test listeners and counts every dial per overlay address.
type locatorFixture struct {
	l       *tailnetLocator
	mu      sync.Mutex
	targets map[string]string // overlay IP -> 127.0.0.1:port
	dials   map[string]int
	snap    tailnet.Snapshot
	snapErr error
	now     time.Time
}

func newLocatorFixture(t *testing.T, mesh *clustertrust.Mesh) *locatorFixture {
	t.Helper()
	f := &locatorFixture{targets: map[string]string{}, dials: map[string]int{}, now: time.Unix(1_800_000_000, 0)}
	f.l = &tailnetLocator{
		status: func(context.Context) (tailnet.Snapshot, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.snap, f.snapErr
		},
		tlsConfig: func() (*tls.Config, bool) { mesh.Refresh(); return mesh.ClientTLSConfigAny() },
		selfUUID:  func() string { return uuidSelf },
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			f.mu.Lock()
			f.dials[host]++
			target := f.targets[host]
			f.mu.Unlock()
			if target == "" {
				return nil, errors.New("connection refused")
			}
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
		emPort:   defaultEngineManagerPort,
		now:      func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		kick:     make(chan struct{}, 1),
		verified: map[string]tailnetVerified{},
		backoff:  map[string]tailnetBackoff{},
		hints:    map[string]tailnetHint{},
	}
	return f
}

func (f *locatorFixture) dialCount(ip string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials[ip]
}

func (f *locatorFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// clusterFixture builds a clustered self with a pinned member (memberDir holds
// the member's keypair) and an unpinned outsider keypair.
func clusterFixture(t *testing.T) (mesh *clustertrust.Mesh, selfDir, memberDir, outsiderDir string) {
	t.Helper()
	root := t.TempDir()
	selfDir = filepath.Join(root, "self", "cluster")
	memberDir = filepath.Join(root, "member", "cluster")
	outsiderDir = filepath.Join(root, "outsider", "cluster")
	clustertrusttest.Join(t, selfDir, "cluster-1", uuidSelf)
	clustertrusttest.WriteKeypair(t, memberDir, uuidMember)
	clustertrusttest.WriteKeypair(t, outsiderDir, uuidOutside)
	pinCert(t, selfDir, memberDir, uuidMember)
	return clustertrust.Open(selfDir), selfDir, memberDir, outsiderDir
}

func TestProveAcceptsAPinnedMember(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr

	uuid, _, ok := f.l.prove(context.Background(), testOverlay(2), tailnetProveTimeout)
	if !ok || uuid != uuidMember {
		t.Fatalf("prove = (%q, %v), want (%q, true)", uuid, ok, uuidMember)
	}
}

// TestProveRejectsAnUnpinnedServerWithoutSendingOurLeaf: a TLS listener whose
// certificate is not a pinned member is refused, and the refusal happens before
// this node presents its own certificate.
func TestProveRejectsAnUnpinnedServerWithoutSendingOurLeaf(t *testing.T) {
	mesh, _, _, outsiderDir := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	peer := startTLSPeer(t, outsiderDir)
	f.targets[testOverlay(3)] = peer.addr

	uuid, definitive, ok := f.l.prove(context.Background(), testOverlay(3), tailnetProveTimeout)
	if ok {
		t.Fatalf("prove accepted an unpinned server as %q", uuid)
	}
	if !definitive {
		t.Fatal("an unpinned certificate is a definite answer, not a transient failure")
	}
	peer.waitAttempt(t)
	if peer.sawClientCert.Load() {
		t.Fatal("this node presented its certificate to a server it had not verified")
	}
}

func TestProveRejectsThisNodeItself(t *testing.T) {
	mesh, selfDir, _, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(4)] = startTLSPeer(t, selfDir).addr

	uuid, definitive, ok := f.l.prove(context.Background(), testOverlay(4), tailnetProveTimeout)
	if ok {
		t.Fatalf("prove filed this node's own address as member %q", uuid)
	}
	if !definitive {
		t.Fatal("this node answering is a definite answer")
	}
}

// TestProveDoesNothingWhileUnclustered: a node that has left its cluster keeps
// its keypair but has no admission and no pins (clustertrust.Mesh.Clustered);
// it must not dial anyone on the tailnet.
func TestProveDoesNothingWhileUnclustered(t *testing.T) {
	root := t.TempDir()
	selfDir := filepath.Join(root, "self", "cluster")
	memberDir := filepath.Join(root, "member", "cluster")
	clustertrusttest.WriteKeypair(t, selfDir, uuidSelf)
	clustertrusttest.WriteKeypair(t, memberDir, uuidMember)
	f := newLocatorFixture(t, clustertrust.Open(selfDir))
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}

	if _, _, ok := f.l.prove(context.Background(), testOverlay(2), tailnetProveTimeout); ok {
		t.Fatal("an unclustered node located a member")
	}
	f.l.refresh(context.Background())
	if n := f.dialCount(testOverlay(2)); n != 0 {
		t.Fatalf("an unclustered node dialed the tailnet %d times", n)
	}
	if got := f.l.current(); len(got) != 0 {
		t.Fatalf("an unclustered node holds hints %+v", got)
	}
}

func TestRefreshLocatesMembersAndRemembersAnswers(t *testing.T) {
	mesh, _, memberDir, outsiderDir := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	member := startTLSPeer(t, memberDir)
	f.targets[testOverlay(2)] = member.addr
	f.targets[testOverlay(3)] = startTLSPeer(t, outsiderDir).addr
	f.snap = tailnet.Snapshot{
		Running:  true,
		SelfIPv4: []string{testOverlay(1)},
		Peers: []tailnet.Peer{
			{HostName: "member-host", IPv4: []string{testOverlay(2)}, Online: true},
			{HostName: "phone", IPv4: []string{testOverlay(3)}, Online: true},
			{HostName: "laptop-off", IPv4: []string{testOverlay(5)}, Online: false},
			{HostName: "self-echo", IPv4: []string{testOverlay(1)}, Online: true},
		},
	}

	f.l.refresh(context.Background())
	hints := f.l.current()
	if len(hints) != 1 || hints[uuidMember].ip != testOverlay(2) || hints[uuidMember].hostName != "member-host" {
		t.Fatalf("hints = %+v, want only the member, at its overlay address", hints)
	}
	for _, ip := range []string{testOverlay(5), testOverlay(1)} {
		if n := f.dialCount(ip); n != 0 {
			t.Fatalf("dialed %s %d times; offline peers and this node's own address are never dialed", ip, n)
		}
	}

	// The member is proven again on every refresh (its proof is its liveness);
	// the device that proved not to be a member is not dialed again.
	f.l.refresh(context.Background())
	if n := f.dialCount(testOverlay(2)); n != 2 {
		t.Fatalf("member dialed %d times across two refreshes, want 2 (re-proven each refresh)", n)
	}
	if n := f.dialCount(testOverlay(3)); n != 1 {
		t.Fatalf("non-member dialed %d times across two refreshes, want 1 (rejection remembered)", n)
	}
}

// TestAMemberThatStopsAnsweringIsDroppedAndRetried: a member whose PAIR stops
// leaves the hint set at the next refresh (so the browser can age it out), and is
// asked again at the following one rather than parked like a non-member.
func TestAMemberThatStopsAnsweringIsDroppedAndRetried(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	addr := startTLSPeer(t, memberDir).addr
	f.targets[testOverlay(2)] = addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background())

	f.mu.Lock()
	f.targets[testOverlay(2)] = "" // PAIR stopped; the device is still on the tailnet
	f.mu.Unlock()
	if retry := f.l.refresh(context.Background()); !retry {
		t.Fatal("a member missing its proof did not ask for a quick retry")
	}
	if got := f.l.current(); len(got) != 0 {
		t.Fatalf("a member that stopped answering is still located: %+v", got)
	}

	f.mu.Lock()
	f.targets[testOverlay(2)] = addr
	f.mu.Unlock()
	f.l.refresh(context.Background())
	if _, ok := f.l.current()[uuidMember]; !ok {
		t.Fatal("a member that answers again was not re-located on the next refresh")
	}
	if n := f.dialCount(testOverlay(2)); n != 3 {
		t.Fatalf("member dialed %d times over three refreshes, want 3 (never parked)", n)
	}
}

func TestRefreshDropsEverythingWhenTheTailnetStops(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background())
	if len(f.l.current()) != 1 {
		t.Fatal("member not located")
	}

	f.snap = tailnet.Snapshot{Running: false}
	f.l.refresh(context.Background())
	if got := f.l.current(); len(got) != 0 {
		t.Fatalf("hints %+v survived a stopped tailnet", got)
	}
}

func TestStatusErrorKeepsOnlyRecentProof(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background())

	f.snapErr = errors.New("tailscale status exited 1")
	f.l.refresh(context.Background())
	if _, ok := f.l.current()[uuidMember]; !ok {
		t.Fatal("a CLI hiccup dropped a member proven moments ago")
	}

	f.advance(tailnetStaleAfter + time.Second)
	f.l.refresh(context.Background())
	if got := f.l.current(); len(got) != 0 {
		t.Fatalf("hints %+v outlived their proof while the tailnet was unreadable", got)
	}
}

func TestAddressesForIsNilSafe(t *testing.T) {
	var l *tailnetLocator
	if got := l.addressesFor(uuidMember); got != nil {
		t.Fatalf("nil locator returned %v", got)
	}
	l.poke() // must not panic
}

func TestTailnetDisabledByEnv(t *testing.T) {
	for _, v := range []string{"0", "false", "OFF", " no "} {
		t.Setenv(tailnetDisableEnv, v)
		if !tailnetDisabled() {
			t.Errorf("%s=%q did not disable the locator", tailnetDisableEnv, v)
		}
	}
	for _, v := range []string{"", "1", "true", "on"} {
		t.Setenv(tailnetDisableEnv, v)
		if tailnetDisabled() {
			t.Errorf("%s=%q disabled the locator", tailnetDisableEnv, v)
		}
	}
}

// TestANonAnsweringDeviceBacksOffInsteadOfBeingParked: no answer proves nothing,
// so a device that refuses is asked again after one refresh interval, then after
// twice that, rather than being written off for ten minutes.
func TestANonAnsweringDeviceBacksOffInsteadOfBeingParked(t *testing.T) {
	mesh, _, _, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "nas", IPv4: []string{testOverlay(9)}, Online: true}}}

	f.l.refresh(context.Background()) // refused: backoff one interval
	f.advance(tailnetRefreshInterval + time.Second)
	f.l.refresh(context.Background()) // asked again, refused again: backoff two intervals
	if n := f.dialCount(testOverlay(9)); n != 2 {
		t.Fatalf("dialed %d times after one interval, want 2", n)
	}
	f.advance(tailnetRefreshInterval + time.Second)
	f.l.refresh(context.Background())
	if n := f.dialCount(testOverlay(9)); n != 2 {
		t.Fatalf("dialed %d times inside the doubled backoff, want 2", n)
	}
	f.advance(tailnetRefreshInterval)
	f.l.refresh(context.Background())
	if n := f.dialCount(testOverlay(9)); n != 3 {
		t.Fatalf("dialed %d times after the doubled backoff, want 3", n)
	}
}

// TestAMemberWhoseFirstProofFailsIsFoundOnTheNextTry: a handshake that times out
// over a relayed path on a cold start must not hide a member for ten minutes.
func TestAMemberWhoseFirstProofFailsIsFoundOnTheNextTry(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background()) // no answer yet

	f.mu.Lock()
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.mu.Unlock()
	f.advance(tailnetRefreshInterval + time.Second)
	f.l.refresh(context.Background())
	if _, ok := f.l.current()[uuidMember]; !ok {
		t.Fatal("member not located one interval after its first proof failed")
	}
}

// TestAProofVouchesForOneMissedRefresh: a member that misses one proof is no
// longer in the hint set, but its last proof still vouches for it (and still
// names its overlay address) until tailnetStaleAfter.
func TestAProofVouchesForOneMissedRefresh(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background())

	f.mu.Lock()
	f.targets[testOverlay(2)] = ""
	f.mu.Unlock()
	f.advance(tailnetRefreshInterval)
	f.l.refresh(context.Background())
	if _, ok := f.l.current()[uuidMember]; ok {
		t.Fatal("precondition: the missed proof should drop the hint")
	}
	if !f.l.vouches(uuidMember) {
		t.Fatal("one missed proof stopped vouching for a member proven a refresh ago")
	}
	if got := f.l.addressesFor(uuidMember); len(got) != 1 || got[0] != testOverlay(2) {
		t.Fatalf("addressesFor = %v, want the last proven address", got)
	}

	f.advance(tailnetStaleAfter)
	if f.l.vouches(uuidMember) || f.l.addressesFor(uuidMember) != nil {
		t.Fatal("a proof older than tailnetStaleAfter still vouches")
	}
}
