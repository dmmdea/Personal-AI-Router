// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tailnet

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
)

// v4 builds an IPv4 address from octets. The fixtures below are synthetic
// overlay (100.64/10) addresses; building them keeps any address literal out of
// the test source.
func v4(a, b, c, d byte) string { return net.IPv4(a, b, c, d).String() }

var (
	ipSelf    = v4(100, 64, 0, 10)
	ipNodeA   = v4(100, 101, 0, 3)
	ipNodeB   = v4(100, 100, 1, 2)
	ipShared  = v4(100, 120, 0, 4)
	ipExpired = v4(100, 110, 0, 5)
)

// statusFixture mirrors the shape of a real `tailscale status --json` document
// (fields this package ignores included), with invented names and addresses.
var statusFixture = strings.NewReplacer(
	"@SELF@", ipSelf, "@A@", ipNodeA, "@B@", ipNodeB, "@SHARED@", ipShared, "@EXPIRED@", ipExpired,
).Replace(`{
  "Version": "1.102.4",
  "BackendState": "Running",
  "TailscaleIPs": ["@SELF@", "fd7a:115c:a1e0::a"],
  "Self": {"ID": "n1", "HostName": "node-self", "DNSName": "node-self.example.ts.net.",
           "TailscaleIPs": ["@SELF@", "fd7a:115c:a1e0::a"], "Online": true, "OS": "windows"},
  "Peer": {
    "nodekey:bbb": {"HostName": "node-b", "DNSName": "node-b.example.ts.net.",
                    "TailscaleIPs": ["@B@", "fd7a:115c:a1e0::b"], "Online": true, "OS": "linux"},
    "nodekey:aaa": {"HostName": "node-a", "DNSName": "node-a.example.ts.net.",
                    "TailscaleIPs": ["fd7a:115c:a1e0::c", "@A@"], "Online": false, "OS": "linux",
                    "Tags": ["tag:server"]},
    "nodekey:ccc": {"HostName": "node-c", "DNSName": "node-c.example.ts.net.",
                    "TailscaleIPs": ["192.0.2.7"], "Online": true},
    "nodekey:ddd": {"HostName": "node-shared", "DNSName": "node-shared.other.ts.net.",
                    "TailscaleIPs": ["@SHARED@"], "Online": true, "AltSharerUserID": 1234567890123},
    "nodekey:eee": {"HostName": "node-expired", "DNSName": "node-expired.example.ts.net.",
                    "TailscaleIPs": ["@EXPIRED@"], "Online": false, "Expired": true}
  },
  "User": {"1": {"LoginName": "someone@example.com"}},
  "MagicDNSSuffix": "example.ts.net"
}`)

func TestParseReadsSelfAndPeersIPv4Only(t *testing.T) {
	snap, err := Parse([]byte(statusFixture))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !snap.Running {
		t.Fatal("Running = false for BackendState Running")
	}
	if want := []string{ipSelf}; !reflect.DeepEqual(snap.SelfIPv4, want) {
		t.Fatalf("SelfIPv4 = %v, want %v", snap.SelfIPv4, want)
	}
	want := []Peer{
		{HostName: "node-a", DNSName: "node-a.example.ts.net.", IPv4: []string{ipNodeA}, Online: false},
		{HostName: "node-b", DNSName: "node-b.example.ts.net.", IPv4: []string{ipNodeB}, Online: true},
		// A non-overlay address is never handed back as a tailnet address.
		{HostName: "node-c", DNSName: "node-c.example.ts.net.", IPv4: nil, Online: true},
		{HostName: "node-expired", DNSName: "node-expired.example.ts.net.", IPv4: []string{ipExpired}, Expired: true},
		{HostName: "node-shared", DNSName: "node-shared.other.ts.net.", IPv4: []string{ipShared}, Online: true, SharedIn: true},
	}
	if !reflect.DeepEqual(snap.Peers, want) {
		t.Fatalf("Peers = %#v\nwant %#v", snap.Peers, want)
	}
}

func TestParseNotRunningIsAStateNotAnError(t *testing.T) {
	for _, state := range []string{"Stopped", "NeedsLogin", "Starting", "NoState", ""} {
		doc := `{"BackendState": "` + state + `", "Peer": {"nodekey:x": {"HostName": "n", "TailscaleIPs": ["` + v4(100, 64, 0, 2) + `"], "Online": true}}}`
		snap, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%q: Parse error %v", state, err)
		}
		if snap.Running || len(snap.Peers) != 0 {
			t.Fatalf("%q: got Running=%v peers=%d, want not running and no peers", state, snap.Running, len(snap.Peers))
		}
	}
}

func TestParseRejectsMalformedDocument(t *testing.T) {
	if _, err := Parse([]byte(`Tailscale is stopped.`)); err == nil {
		t.Fatal("Parse accepted a non-JSON document")
	}
}

func TestIsOverlayIPv4Bounds(t *testing.T) {
	cases := map[string]bool{
		v4(100, 64, 0, 0):             true, // bottom of 100.64/10
		v4(100, 127, 255, 255):        true, // top of 100.64/10
		v4(100, 96, 1, 1):             true,
		v4(100, 63, 255, 255):         false,
		v4(100, 128, 0, 0):            false,
		"10.1.2.3":                    false,
		"192.0.2.1":                   false,
		"fd7a:115c:a1e0::1":           false,
		"::ffff:" + v4(100, 64, 0, 1): true, // IPv4-mapped form of an overlay address
		"not-an-ip":                   false,
		"":                            false,
	}
	for in, want := range cases {
		if got := IsOverlayIPv4(in); got != want {
			t.Errorf("IsOverlayIPv4(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStatusWithoutClientIsUnavailable(t *testing.T) {
	if _, err := Status(context.Background(), nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Status(nil runner) err = %v, want ErrUnavailable", err)
	}
}

func TestStatusPropagatesRunnerError(t *testing.T) {
	boom := errors.New("boom")
	_, err := Status(context.Background(), func(context.Context) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("Status err = %v, want runner error", err)
	}
}

func TestCappedBufferRecordsOverflow(t *testing.T) {
	b := &cappedBuffer{limit: 4}
	if n, _ := b.Write([]byte("abc")); n != 3 || b.overflow {
		t.Fatalf("first write: n=%d overflow=%v", n, b.overflow)
	}
	if n, _ := b.Write([]byte("defg")); n != 4 || !b.overflow {
		t.Fatalf("second write: n=%d overflow=%v, want overflow", n, b.overflow)
	}
	if got := b.String(); got != "abcd" {
		t.Fatalf("kept %q, want %q", got, "abcd")
	}
}
