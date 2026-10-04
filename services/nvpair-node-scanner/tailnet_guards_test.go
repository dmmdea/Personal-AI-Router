// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
	"nvpair-shared/tailnet"
)

// TestMergeIgnoresARecordSittingOnTheMemberKey: the browser keys a record without
// uuid= by its instance name, so a record named like a member's UUID lands on the
// member's key. It must not count as hearing the member.
func TestMergeIgnoresARecordSittingOnTheMemberKey(t *testing.T) {
	m := newTailnetMerge(located)
	squatter := RawNode{ID: uuidMember, Addresses: []string{"192.0.2.66"}, TXT: []string{"v=1", "ni=14318"}}
	got := m.supplement(map[string]RawNode{uuidMember: squatter})[uuidMember]
	if !isOverlayRecord(got) || noderec.ParseTXT(got.TXT).HostUUID != uuidMember {
		t.Fatalf("the member's key holds %+v, want the member's own overlay record", got)
	}
}

// TestOverlayRecordNeverTakesPortsFromMDNS: an mDNS record is unauthenticated, so
// the ports a proven member is dialed on are the fixed cluster ports, whatever its
// last heard record claimed.
func TestOverlayRecordNeverTakesPortsFromMDNS(t *testing.T) {
	spoofed := noderec.NodeRecord{HostUUID: uuidMember, ClusterUUID: "not-the-member", IP: "192.168.1.20",
		Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: 22, noderec.ServiceEngineManager: 445}}
	m := newTailnetMerge(located)
	m.supplement(map[string]RawNode{uuidMember: {ID: "MemberHost", Addresses: []string{"192.168.1.20"}, TXT: spoofed.TXT()}})
	var got RawNode
	for i := 0; i < tailnetCarryScans; i++ {
		got = m.supplement(map[string]RawNode{})[uuidMember]
	}
	rec := noderec.ParseTXT(got.TXT)
	if !reflect.DeepEqual(rec.Services, defaultClusterPorts) {
		t.Fatalf("overlay record ports %v, want the fixed cluster ports", rec.Services)
	}
	if rec.ClusterUUID != uuidMember {
		t.Fatalf("overlay record cluster principal %q, want the proven UUID", rec.ClusterUUID)
	}
	if got.ID != "MemberHost" {
		t.Fatalf("overlay record named %q, want the last heard instance name (display only)", got.ID)
	}
}

// TestSharedInExpiredAndUnprovenOfflinePeersAreNeverDialed: a device shared in
// from another tailnet, an expired one, and an offline one that never proved to
// be a member are not asked at all.
func TestSharedInExpiredAndUnprovenOfflinePeersAreNeverDialed(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	member := startTLSPeer(t, memberDir).addr
	for _, ip := range []string{testOverlay(20), testOverlay(21), testOverlay(22)} {
		f.targets[ip] = member // would prove as the member if it were ever dialed
	}
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{
		{HostName: "foreign", IPv4: []string{testOverlay(20)}, Online: true, SharedIn: true},
		{HostName: "expired", IPv4: []string{testOverlay(21)}, Online: true, Expired: true},
		{HostName: "asleep", IPv4: []string{testOverlay(22)}, Online: false},
	}}
	f.l.refresh(context.Background())
	for _, ip := range []string{testOverlay(20), testOverlay(21), testOverlay(22)} {
		if n := f.dialCount(ip); n != 0 {
			t.Errorf("%s dialed %d times", ip, n)
		}
	}
}

// TestAnOfflineFlaggedMemberIsStillAskedOnceProven: the control plane's Online
// flag goes stale when this node's own connection degrades, so a device that
// proved to be a member is asked even while it is reported offline.
func TestAnOfflineFlaggedMemberIsStillAskedOnceProven(t *testing.T) {
	mesh, _, memberDir, _ := clusterFixture(t)
	f := newLocatorFixture(t, mesh)
	f.targets[testOverlay(2)] = startTLSPeer(t, memberDir).addr
	f.snap = tailnet.Snapshot{Running: true, Peers: []tailnet.Peer{{HostName: "m", IPv4: []string{testOverlay(2)}, Online: true}}}
	f.l.refresh(context.Background())

	f.snap.Peers[0].Online = false
	f.l.refresh(context.Background())
	if _, ok := f.l.current()[uuidMember]; !ok {
		t.Fatal("a proven member reported offline by the control plane was not asked")
	}
}

// overlayRoutedDaemon is a daemon whose node-info client sends every request,
// whatever address it names, to srv.
func overlayRoutedDaemon(srv *httptest.Server) *daemon {
	target := strings.TrimPrefix(srv.URL, "http://")
	dialer := &net.Dialer{}
	return &daemon{
		http: &http.Client{Timeout: nodeInfoFetchTimeout, Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, target)
			},
		}},
		lastInfo:     map[string]NodeInfoResponse{},
		lastInfoAt:   map[string]time.Time{},
		nodeInfoDown: map[string]bool{},
	}
}

func nodeInfoAnswering(t *testing.T, resp NodeInfoResponse) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEnrichmentAtAnOverlayAddressRequiresTheHostUUID: with the tailnet down, a
// dial to 100.64/10 leaves through the default route of whatever network this
// node is on; an answer there that does not name the node is not the node.
func TestEnrichmentAtAnOverlayAddressRequiresTheHostUUID(t *testing.T) {
	gpus := []GPUInfo{{Name: "spoofed"}}
	node := noderec.DirectoryNode{HostUUID: uuidMember,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14318}}}

	d := overlayRoutedDaemon(nodeInfoAnswering(t, NodeInfoResponse{GPUs: gpus}))
	n := node
	d.enrichInfoCandidates(&n, []string{overlayIP})
	if len(n.GPUs) != 0 {
		t.Fatal("telemetry from an overlay address that named no host was accepted")
	}

	d = overlayRoutedDaemon(nodeInfoAnswering(t, NodeInfoResponse{GPUs: []GPUInfo{{Name: "real"}}, HostUUID: uuidMember}))
	n = node
	d.enrichInfoCandidates(&n, []string{overlayIP})
	if len(n.GPUs) != 1 || n.GPUs[0].Name != "real" {
		t.Fatalf("telemetry from the member at its overlay address was refused: %+v", n.GPUs)
	}
}

// TestPeerAddressesLeaveOutProvenOverlayAddresses: a member's overlay address is
// not evidence that a physical interface faces peers.
func TestPeerAddressesLeaveOutProvenOverlayAddresses(t *testing.T) {
	now := time.Now()
	d := &daemon{dir: newDirectory(), tailnet: vouchingLocator(uuidMember, overlayIP, now, &now)}
	d.dir.upsert(noderec.DirectoryNode{HostUUID: uuidMember, IP: "192.168.1.20", IPs: []string{"192.168.1.20", overlayIP}})
	got := d.peerAddresses()
	if slices.Contains(got, overlayIP) || !slices.Contains(got, "192.168.1.20") {
		t.Fatalf("peerAddresses = %v, want the LAN address without the overlay one", got)
	}
}

// TestSupplementSurvivesAFault: a fault in the merge costs one scan's supplement,
// never the scanner.
func TestSupplementSurvivesAFault(t *testing.T) {
	m := newTailnetMerge(func() map[string]tailnetHint { panic("boom") })
	in := map[string]RawNode{uuidMember: heardRecord()}
	out := m.supplement(in)
	if !reflect.DeepEqual(out[uuidMember], heardRecord()) || len(out) != 1 {
		t.Fatalf("a faulted merge returned %+v, want the mDNS sightings unchanged", out)
	}
}

// TestLocatorSwitchesOffAfterRepeatedFaults: refreshes that keep faulting are
// recovered, and after tailnetMaxFaults the locator stops instead of looping.
func TestLocatorSwitchesOffAfterRepeatedFaults(t *testing.T) {
	saved := tailnetQuickRetries
	tailnetQuickRetries = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { tailnetQuickRetries = saved })

	calls := 0
	l := &tailnetLocator{
		status:    func(context.Context) (tailnet.Snapshot, error) { calls++; panic(errors.New("boom")) },
		tlsConfig: func() (*tls.Config, bool) { return &tls.Config{}, true },
		now:       time.Now,
		kick:      make(chan struct{}, 1),
		hints:     map[string]tailnetHint{}, verified: map[string]tailnetVerified{}, backoff: map[string]tailnetBackoff{},
	}
	done := make(chan struct{})
	go func() { l.run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a locator whose refresh keeps faulting never switched off")
	}
	if calls != tailnetMaxFaults {
		t.Fatalf("refresh ran %d times before switching off, want %d", calls, tailnetMaxFaults)
	}
}
