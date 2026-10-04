// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

// vouchingLocator is a locator whose only state is one proof of uuid at ip.
func vouchingLocator(uuid, ip string, provenAt time.Time, now *time.Time) *tailnetLocator {
	return &tailnetLocator{
		verified: map[string]tailnetVerified{ip: {uuid: uuid, at: provenAt}},
		backoff:  map[string]tailnetBackoff{},
		hints:    map[string]tailnetHint{},
		now:      func() time.Time { return *now },
	}
}

// TestOverlayRecordLivenessIsThePinnedProof: a record built from a locator proof
// is kept exactly while that proof vouches for the member — never on a plain
// node-info answer, which anyone holding the address could give.
func TestOverlayRecordLivenessIsThePinnedProof(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	d := &daemon{tailnet: vouchingLocator(uuidMember, overlayIP, now, &now)}
	m := newTailnetMerge(func() map[string]tailnetHint {
		return map[string]tailnetHint{uuidMember: {uuid: uuidMember, ip: overlayIP, hostName: "m"}}
	})
	rec := m.overlayRecord(uuidMember, tailnetHint{uuid: uuidMember, ip: overlayIP, hostName: "m"})

	if !d.reachable(rec) {
		t.Fatal("a freshly proven member's overlay record failed its liveness check")
	}
	now = now.Add(tailnetStaleAfter + time.Second)
	if d.reachable(rec) {
		t.Fatal("an overlay record outlived the proof that created it")
	}
}

// TestSupersedingUpsertKeepsAVouchedMember: the identity proof that evicts a
// ghost (an arriving machine naming a different UUID at the old record's name)
// must not evict a member that just proved itself with its pinned certificate.
func TestSupersedingUpsertKeepsAVouchedMember(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.1")
	d.dir.upsert(withNodeInfo(named("live-uuid", "wiped-host", loopbackHost, 3600), closedPort(t)))
	_, wipedPort := nodeInfoServer(t, "claimant-uuid")
	claim := withNodeInfo(named("claimant-uuid", "wiped-host", loopbackHost, 0), wipedPort)

	now := time.Now()
	d.tailnet = vouchingLocator("live-uuid", overlayIP, now, &now)
	d.supersedingUpsert(claim)
	if _, ok := d.dir.get("live-uuid"); !ok {
		t.Fatal("a member vouched for by its pinned certificate was evicted as a ghost")
	}
}
