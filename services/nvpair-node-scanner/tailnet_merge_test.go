// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"reflect"
	"slices"
	"testing"

	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

// testOverlay returns a synthetic overlay (tailnet) IPv4 address in 100.64/10,
// built from octets so test fixtures carry no address literal.
func testOverlay(host byte) string { return net.IPv4(100, 64, 0, host).String() }

var overlayIP = testOverlay(2)

// heardRecord is a member's mDNS record as the browser reports it, with the
// inference ports a live member advertises.
func heardRecord() RawNode {
	rec := noderec.NodeRecord{
		HostUUID: uuidMember, ClusterUUID: uuidMember, IP: "192.168.1.20",
		IPs: []string{"192.168.1.20", "172.17.5.1"},
		Services: map[noderec.ServiceKey]int{
			noderec.ServiceNodeInfo: 14318, noderec.ServiceOllama: 11434, noderec.ServiceLMStudio: 1234,
			noderec.ServiceErrors: 14319, noderec.ServiceWorkload: 14320, noderec.ServiceCluster: 14321,
			noderec.ServiceEngineManager: 14322, noderec.ServiceEngineControl: 14323,
		},
	}
	return RawNode{ID: "MemberHost", Host: "MemberHost.local.", Port: noderec.SRVPort,
		Addresses: []string{"192.168.1.20"}, TXT: rec.TXT()}
}

func located() map[string]tailnetHint {
	return map[string]tailnetHint{uuidMember: {uuid: uuidMember, ip: overlayIP, hostName: "memberhost-ts"}}
}

func TestMergeAddsTheOverlayAddressToAHeardMember(t *testing.T) {
	m := newTailnetMerge(located)
	out := m.supplement(map[string]RawNode{uuidMember: heardRecord()})
	got := out[uuidMember]
	if !slices.Contains(got.Addresses, overlayIP) {
		t.Fatalf("addresses %v lack the overlay address", got.Addresses)
	}
	if !reflect.DeepEqual(got.TXT, heardRecord().TXT) || got.ID != "MemberHost" {
		t.Fatal("a heard member's own record was rewritten; only the address may be added")
	}
}

// TestMergeCarriesTheLastRecordThroughAShortSilence: dropped multicast answers
// must not change the record (no update storm), and a sustained silence switches
// once to the overlay record carrying the member's real name and ports.
func TestMergeCarriesTheLastRecordThroughAShortSilence(t *testing.T) {
	m := newTailnetMerge(located)
	first := m.supplement(map[string]RawNode{uuidMember: heardRecord()})[uuidMember]

	for i := 1; i < tailnetCarryScans; i++ {
		carried := m.supplement(map[string]RawNode{})[uuidMember]
		if !reflect.DeepEqual(carried, first) {
			t.Fatalf("silent scan %d changed the record:\n got %+v\nwant %+v", i, carried, first)
		}
	}

	flipped := m.supplement(map[string]RawNode{})[uuidMember]
	if reflect.DeepEqual(flipped, first) {
		t.Fatal("record never switched to the overlay address after a sustained silence")
	}
	rec := noderec.ParseTXT(flipped.TXT)
	if rec.IP != overlayIP || !reflect.DeepEqual(flipped.Addresses, []string{overlayIP}) {
		t.Fatalf("overlay record ip=%q addrs=%v, want the overlay address only", rec.IP, flipped.Addresses)
	}
	if len(rec.IPs) != 0 || slices.Contains(netpick.Candidates(flipped.TXT, flipped.Addresses), "192.168.1.20") {
		t.Fatal("overlay record still names the LAN address it was heard on")
	}
	if flipped.ID != "MemberHost" {
		t.Fatalf("overlay record named %q, want the member's own instance name", flipped.ID)
	}
	want := map[noderec.ServiceKey]int{
		noderec.ServiceNodeInfo: 14318, noderec.ServiceErrors: 14319, noderec.ServiceWorkload: 14320,
		noderec.ServiceCluster: 14321, noderec.ServiceEngineManager: 14322, noderec.ServiceEngineControl: 14323,
	}
	if !reflect.DeepEqual(rec.Services, want) {
		t.Fatalf("overlay record ports %v, want the member's advertised cluster ports %v (ol/lm dropped)", rec.Services, want)
	}
	if !isOverlayRecord(flipped) {
		t.Fatal("overlay record is not tagged; its liveness would fall back to the plaintext probe")
	}
	if rec.HostUUID != uuidMember || rec.ClusterUUID != uuidMember {
		t.Fatalf("overlay record identity uuid=%q cluster=%q", rec.HostUUID, rec.ClusterUUID)
	}
}

func TestMergeDescribesANeverHeardMemberWithTheFixedPorts(t *testing.T) {
	m := newTailnetMerge(located)
	got := m.supplement(map[string]RawNode{})[uuidMember]
	rec := noderec.ParseTXT(got.TXT)
	if got.ID != "memberhost-ts" {
		t.Fatalf("named %q, want the Tailscale hostname", got.ID)
	}
	if rec.HostUUID != uuidMember || rec.ClusterUUID != uuidMember || rec.IP != overlayIP {
		t.Fatalf("record %+v, want the member at its overlay address", rec)
	}
	if !reflect.DeepEqual(rec.Services, defaultClusterPorts) {
		t.Fatalf("ports %v, want the fixed cluster ports", rec.Services)
	}
	for _, s := range []noderec.ServiceKey{noderec.ServiceOllama, noderec.ServiceLMStudio} {
		if _, ok := rec.Services[s]; ok {
			t.Fatalf("a never-heard member advertises %s, whose port is only known from its own record", s)
		}
	}
	if !isOverlayRecord(got) {
		t.Fatal("overlay record is not tagged")
	}
	if p := noderec.ParseTXT(got.TXT); len(p.Services) != len(defaultClusterPorts) {
		t.Fatalf("the provenance tag leaked into the parsed record: %v", p.Services)
	}
}

func TestMergeLeavesUnlocatedNodesAlone(t *testing.T) {
	m := newTailnetMerge(func() map[string]tailnetHint { return nil })
	in := map[string]RawNode{uuidMember: heardRecord()}
	out := m.supplement(in)
	if !reflect.DeepEqual(out[uuidMember], heardRecord()) || len(out) != 1 {
		t.Fatalf("an unlocated member was altered: %+v", out)
	}
	if out := m.supplement(map[string]RawNode{}); len(out) != 0 {
		t.Fatalf("an unheard, unlocated member was invented: %+v", out)
	}
}

func TestMergeComesBackHomeToTheHeardRecord(t *testing.T) {
	m := newTailnetMerge(located)
	m.supplement(map[string]RawNode{uuidMember: heardRecord()})
	for i := 0; i < tailnetCarryScans; i++ {
		m.supplement(map[string]RawNode{})
	}
	back := m.supplement(map[string]RawNode{uuidMember: heardRecord()})[uuidMember]
	if !reflect.DeepEqual(back.TXT, heardRecord().TXT) || !slices.Contains(back.Addresses, "192.168.1.20") {
		t.Fatalf("back home the record is %+v, want the heard one", back)
	}
	// And a later silence starts counting from zero again.
	if again := m.supplement(map[string]RawNode{})[uuidMember]; !reflect.DeepEqual(again, back) {
		t.Fatal("the first silent scan after coming home flipped the record immediately")
	}
}

// TestMergeResumesTheOverlayRecordAfterAMissedProof: away from home, a member
// that drops out of the locator for one refresh comes back on its overlay record
// directly; it is not walked back through its stale LAN record (two address
// flips published for nothing).
func TestMergeResumesTheOverlayRecordAfterAMissedProof(t *testing.T) {
	hints := located()
	m := newTailnetMerge(func() map[string]tailnetHint { return hints })
	m.supplement(map[string]RawNode{uuidMember: heardRecord()})
	var away RawNode
	for i := 0; i < tailnetCarryScans; i++ {
		away = m.supplement(map[string]RawNode{})[uuidMember]
	}
	if noderec.ParseTXT(away.TXT).IP != overlayIP {
		t.Fatal("precondition: member not on its overlay record")
	}

	hints = nil // one refresh without a proof
	if out := m.supplement(map[string]RawNode{}); len(out) != 0 {
		t.Fatalf("an unlocated, unheard member was supplied: %+v", out)
	}
	hints = located()
	if back := m.supplement(map[string]RawNode{})[uuidMember]; !reflect.DeepEqual(back, away) {
		t.Fatalf("after a missed proof the member came back as %+v, want its overlay record", back)
	}
}

// TestTailnetCandidatesKeepTheOverlayInsideTheCap: a member with a full list of
// its own addresses still carries the overlay address, in the last slot inside
// the cap, behind every address it ranked itself (only its lowest-ranked one is
// displaced).
func TestTailnetCandidatesKeepTheOverlayInsideTheCap(t *testing.T) {
	rec := noderec.NodeRecord{HostUUID: uuidMember, IP: "192.168.1.20",
		IPs: []string{"192.168.1.20", "172.17.5.1", "172.20.0.1", "10.8.0.4"}}
	txt := rec.TXT()
	if base := netpick.Candidates(txt, []string{overlayIP}); slices.Contains(base, overlayIP) {
		t.Fatalf("precondition: plain Candidates %v already keeps the overlay address", base)
	}
	got := tailnetCandidates(txt, []string{overlayIP}, []string{overlayIP})
	want := []string{"192.168.1.20", "172.17.5.1", "172.20.0.1", overlayIP}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates %v, want %v", got, want)
	}
}

func TestTailnetCandidatesForAnOverlayRecordIsTheOverlayAlone(t *testing.T) {
	rec := noderec.NodeRecord{HostUUID: uuidMember, IP: overlayIP}
	got := tailnetCandidates(rec.TXT(), []string{overlayIP}, []string{overlayIP})
	if !reflect.DeepEqual(got, []string{overlayIP}) {
		t.Fatalf("candidates %v, want only %s", got, overlayIP)
	}
}

func TestTailnetCandidatesWithoutOverlayIsUnchanged(t *testing.T) {
	txt := heardRecord().TXT
	addrs := []string{"192.168.1.20"}
	if got, want := tailnetCandidates(txt, addrs, nil), netpick.Candidates(txt, addrs); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates %v, want netpick's %v", got, want)
	}
}

func TestToDirectoryNodeKeepsTheOverlayInsideTheCap(t *testing.T) {
	raw := withAddress(heardRecord(), overlayIP)
	node, ok := toDirectoryNode(raw, true, overlayIP)
	if !ok {
		t.Fatal("record rejected")
	}
	want := []string{"192.168.1.20", "172.17.5.1", overlayIP}
	if node.IP != "192.168.1.20" || !reflect.DeepEqual(node.IPs, want) {
		t.Fatalf("directory node ip=%q ips=%v, want ips %v (own order kept, overlay last)", node.IP, node.IPs, want)
	}
}
