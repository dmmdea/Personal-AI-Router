// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureRW is the manager's local interface in these tests: whatever the
// codec writes (the upward workloads:upsert / workloads:remove notifications)
// is captured; Read blocks forever, as an idle broker would.
type captureRW struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureRW) Read(p []byte) (int, error) { select {} }
func (c *captureRW) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}
func (c *captureRW) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func newTestManager(t *testing.T) (*Manager, *captureRW) {
	t.Helper()
	rw := &captureRW{}
	m, err := NewManager(NewCodec(rw), 0, "self-uuid", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.ctx, m.cancel = ctx, cancel
	return m, rw
}

// ingestFrame is well-formed lifecycle params for workload id in state, with no
// origin, as a local producer would send them.
func ingestFrame(id string, state WorkloadState) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"workloadInfo":{"id":%q,"model":"qwen","engine":"llamacpp","runId":%q,"state":%q,"createdAt":1,"startedAt":1,"completedAt":null,"error":null,"requesterId":null}}`, id, id, string(state)))
}

// rpcBody wraps params in the JSON-RPC 2.0 notification a producer POSTs.
func rpcBody(method string, params json.RawMessage) string {
	return `{"jsonrpc":"2.0","method":"` + method + `","params":` + string(params) + `}`
}

func startedBody(id string) string {
	return rpcBody(MethodStarted, ingestFrame(id, StateRunning))
}

// spellingBypasses are frames that slip a check made on exact keys: encoding/json
// reads a key into a field whatever its case (and folds "ſ" to s and
// "K" to k), so each of these decodes as a foreign origin or as the peers'
// resync marker. The ingress refuses every one.
var spellingBypasses = map[string]struct{ method, params string }{
	"originatedfrom beside a blank origin": {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"","originatedfrom":"victim","createdAt":1}}`},
	"originatedfrom alone":                 {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedfrom":"victim","createdAt":1}}`},
	"second workloadinfo object":           {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1},"workloadinfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"victim","createdAt":1}}`},
	"kelvin sign workloadinfo":             {MethodStarted, `{"worKloadinfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"victim","createdAt":1}}`},
	"capitalised id":                       {MethodStarted, `{"workloadInfo":{"ID":"W","model":"m","engine":"e","state":"running","createdAt":1}}`},
	"capitalised state":                    {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","State":"running","createdAt":1}}`},
	"remove originatedfrom":                {MethodRemove, `{"workloadId":"W","originatedfrom":"victim"}`},
	"remove originatedfrom beside blank":   {MethodRemove, `{"workloadId":"W","originatedFrom":"","originatedfrom":"victim"}`},
	"remove capitalised workloadId":        {MethodRemove, `{"WorkloadId":"W"}`},
	"Resync":                               {MethodStarted, `{"Resync":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1}}`},
	"RESYNC":                               {MethodStarted, `{"RESYNC":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1}}`},
	"long-s resync":                        {MethodStarted, `{"reſync":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1}}`},
	"remove Resync":                        {MethodRemove, `{"Resync":true,"workloadId":"W"}`},
}

func TestNormalizeIngressParamsFillsEmptyLifecycleOrigin(t *testing.T) {
	in := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"m","engine":"llamacpp","state":"running","createdAt":1}}`)
	out, err := normalizeIngressParams(MethodStarted, in, "self-uuid")
	if err != nil {
		t.Fatal(err)
	}
	wl, err := parseLifecycle(out)
	if err != nil {
		t.Fatal(err)
	}
	if wl.OriginatedFrom != "self-uuid" {
		t.Fatalf("originatedFrom = %q, want self-uuid", wl.OriginatedFrom)
	}
	if wl.ID != "j1" || wl.Engine != "llamacpp" {
		t.Fatalf("other fields disturbed: %+v", wl)
	}
}

func TestNormalizeIngressParamsFillsNullAndEmptyOrigin(t *testing.T) {
	for name, tc := range map[string]struct{ method, params string }{
		"lifecycle null":  {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","originatedFrom":null}}`},
		"lifecycle empty": {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","originatedFrom":""}}`},
		"remove null":     {MethodRemove, `{"workloadId":"j1","originatedFrom":null}`},
		"remove empty":    {MethodRemove, `{"workloadId":"j1","originatedFrom":""}`},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := normalizeIngressParams(tc.method, json.RawMessage(tc.params), "self-uuid")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), `"originatedFrom":"self-uuid"`) {
				t.Fatalf("origin not stamped: %s", out)
			}
		})
	}
}

func TestNormalizeIngressParamsAcceptsOwnOrigin(t *testing.T) {
	for name, tc := range map[string]struct{ method, params string }{
		"lifecycle": {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"llamacpp","state":"running","originatedFrom":"self-uuid","createdAt":1}}`},
		"remove":    {MethodRemove, `{"workloadId":"j1","originatedFrom":"self-uuid"}`},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := normalizeIngressParams(tc.method, json.RawMessage(tc.params), "self-uuid")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), `"originatedFrom":"self-uuid"`) {
				t.Fatalf("own origin not kept: %s", out)
			}
		})
	}
}

func TestNormalizeIngressParamsRejectsForeignOrigin(t *testing.T) {
	for name, tc := range map[string]struct{ method, params string }{
		"lifecycle":            {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","originatedFrom":"other-node"}}`},
		"lifecycle non-string": {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","originatedFrom":7}}`},
		"remove":               {MethodRemove, `{"workloadId":"j1","originatedFrom":"other-node"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeIngressParams(tc.method, json.RawMessage(tc.params), "self-uuid"); !errors.Is(err, errIngressOrigin) {
				t.Fatalf("err = %v, want errIngressOrigin", err)
			}
		})
	}
}

func TestNormalizeIngressParamsRemove(t *testing.T) {
	out, err := normalizeIngressParams(MethodRemove, json.RawMessage(`{"workloadId":"j1"}`), "self-uuid")
	if err != nil {
		t.Fatal(err)
	}
	id, node, err := parseRemove(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != "j1" || node != "self-uuid" {
		t.Fatalf("remove = (%q, %q), want (j1, self-uuid)", id, node)
	}
}

func TestNormalizeIngressParamsRejectsMissingInfo(t *testing.T) {
	if _, err := normalizeIngressParams(MethodStarted, json.RawMessage(`{}`), "self-uuid"); err == nil {
		t.Fatal("expected an error for a lifecycle frame without workloadInfo")
	}
}

func TestNormalizeIngressParamsRejectsResyncMarker(t *testing.T) {
	for name, tc := range map[string]struct{ method, params string }{
		"lifecycle": {MethodStarted, `{"resync":true,"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","createdAt":1}}`},
		"remove":    {MethodRemove, `{"resync":true,"workloadId":"j1"}`},
		"false":     {MethodRemove, `{"resync":false,"workloadId":"j1"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeIngressParams(tc.method, json.RawMessage(tc.params), "self-uuid"); !errors.Is(err, errIngressResync) {
				t.Fatalf("err = %v, want errIngressResync", err)
			}
		})
	}
}

func TestNormalizeIngressParamsRejectsSpellingBypasses(t *testing.T) {
	for name, tc := range spellingBypasses {
		t.Run(name, func(t *testing.T) {
			if out, err := normalizeIngressParams(tc.method, json.RawMessage(tc.params), "self-uuid"); err == nil {
				t.Fatalf("frame accepted: %s", out)
			}
		})
	}
}

func TestNormalizeIngressParamsRejectsKeysDifferingOnlyByCase(t *testing.T) {
	for name, tc := range map[string]struct{ method, params string }{
		"top level":     {MethodRemove, `{"workloadId":"j1","extra":1,"EXTRA":2}`},
		"workload info": {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","label":"a","LABEL":"b"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeIngressParams(tc.method, json.RawMessage(tc.params), "self-uuid"); err == nil {
				t.Fatal("expected keys that differ only by case to be refused")
			}
		})
	}
}

func TestNormalizeIngressParamsKeepsOtherKeys(t *testing.T) {
	in := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","label":"nightly"},"trace":"t-1"}`)
	out, err := normalizeIngressParams(MethodStarted, in, "self-uuid")
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"label":"nightly"`) || !strings.Contains(s, `"trace":"t-1"`) {
		t.Fatalf("a key the decoders ignore was dropped: %s", s)
	}
}

func TestIngressFieldNamesFollowTheDecoders(t *testing.T) {
	has := func(names []string, want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"workloadInfo", "workloadId", "originatedFrom"} {
		if !has(ingressParamFields, want) {
			t.Errorf("params fields %v lack %q", ingressParamFields, want)
		}
	}
	for _, want := range []string{"id", "model", "engine", "runId", "state", "originatedFrom", "scheduledOn", "createdAt", "startedAt", "completedAt", "error", "requesterId"} {
		if !has(ingressWorkloadFields, want) {
			t.Errorf("workload fields %v lack %q", ingressWorkloadFields, want)
		}
	}
}

func TestFoldKeyAgreesWithEqualFold(t *testing.T) {
	keys := []string{"resync", "Resync", "RESYNC", "reſync", "workloadInfo", "worKloadInfo", "workloadinfo", "i", "I", "ı", "İ", "s", "S", "ſ", "", "id", "ID", "Id", "identity"}
	for _, a := range keys {
		for _, b := range keys {
			if got, want := foldKey(a) == foldKey(b), strings.EqualFold(a, b); got != want {
				t.Errorf("foldKey(%q) == foldKey(%q) is %v, strings.EqualFold says %v", a, b, got, want)
			}
		}
	}
}

func TestCheckIngressWorkloadRejectsDecodedForeignOrigin(t *testing.T) {
	wl := &Workload{ID: "W", Model: "m", Engine: "e", State: StateRunning, OriginatedFrom: "other-node"}
	if err := checkIngressWorkload(wl, "self-uuid"); !errors.Is(err, errIngressOrigin) {
		t.Fatalf("err = %v, want errIngressOrigin", err)
	}
}

func TestCheckIngressRemovalRejectsDecodedForeignOrigin(t *testing.T) {
	if err := checkIngressRemoval("W", "other-node", "self-uuid"); !errors.Is(err, errIngressOrigin) {
		t.Fatalf("err = %v, want errIngressOrigin", err)
	}
}

func TestIngestLocalTracksAndEmitsUpsert(t *testing.T) {
	m, rw := newTestManager(t)
	params := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"qwen","engine":"llamacpp","runId":"j1","state":"running","originatedFrom":"self-uuid","createdAt":5,"startedAt":5,"completedAt":null,"error":null,"requesterId":null}}`)
	if err := m.ingestLocal(MethodStarted, params); err != nil {
		t.Fatal(err)
	}
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1", got)
	}
	out := rw.String()
	if !strings.Contains(out, `"method":"workloads:upsert"`) || !strings.Contains(out, `"id":"j1"`) {
		t.Fatalf("broker did not receive the upsert: %s", out)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"j1","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
	if out := rw.String(); !strings.Contains(out, `"method":"workloads:remove"`) {
		t.Fatalf("broker did not receive the remove: %s", out)
	}
}

func TestIngestLocalRejectsMalformed(t *testing.T) {
	m, rw := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, json.RawMessage(`{"workloadInfo":{"id":""}}`)); err == nil {
		t.Fatal("expected validation error")
	}
	if err := m.ingestLocal("bogus:method", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected unknown-method error")
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("malformed frames must not be tracked, got %d", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("malformed frames must not reach the broker: %s", out)
	}
}

func TestIngestLocalRejectsForeignOrigin(t *testing.T) {
	m, rw := newTestManager(t)
	foreign := json.RawMessage(`{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"other-node","createdAt":1}}`)
	if err := m.ingestLocal(MethodStarted, foreign); !errors.Is(err, errIngressOrigin) {
		t.Fatalf("lifecycle err = %v, want errIngressOrigin", err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"other-node"}`)); !errors.Is(err, errIngressOrigin) {
		t.Fatalf("remove err = %v, want errIngressOrigin", err)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a foreign-origin frame reached the broker: %s", out)
	}
}

func TestIngestLocalRejectsSpellingBypasses(t *testing.T) {
	m, rw := newTestManager(t)
	for name, tc := range spellingBypasses {
		if err := m.ingestLocal(tc.method, json.RawMessage(tc.params)); err == nil {
			t.Errorf("%s: frame accepted, want an error", name)
		}
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a refused frame reached the broker: %s", out)
	}
}

func TestIngestLocalRejectsResyncMarker(t *testing.T) {
	m, rw := newTestManager(t)
	params := json.RawMessage(`{"resync":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1}}`)
	if err := m.ingestLocal(MethodStarted, params); !errors.Is(err, errIngressResync) {
		t.Fatalf("err = %v, want errIngressResync", err)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a frame carrying resync reached the broker: %s", out)
	}
}

func TestIngestLocalRejectsUnknownState(t *testing.T) {
	m, rw := newTestManager(t)
	for _, state := range []string{"done", "complete", "Running", "paused", "bogus"} {
		params := json.RawMessage(fmt.Sprintf(`{"workloadInfo":{"id":"W","model":"m","engine":"e","state":%q,"originatedFrom":"self-uuid","createdAt":1}}`, state))
		if err := m.ingestLocal(MethodStarted, params); err == nil {
			t.Errorf("state %q accepted, want an error", state)
		}
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("an unknown state reached the broker: %s", out)
	}
}

func TestIngestLocalAcceptsEveryKnownState(t *testing.T) {
	m, _ := newTestManager(t)
	for _, state := range []WorkloadState{StateInitializing, StateQueued, StateRunning, StateCompleted, StateFailed} {
		if err := m.ingestLocal(MethodStarted, ingestFrame(string(state), state)); err != nil {
			t.Errorf("state %q refused: %v", state, err)
		}
	}
}

func TestIngestLocalRejectsOversizeID(t *testing.T) {
	m, rw := newTestManager(t)
	long := strings.Repeat("a", localIngressMaxIDLen+1)
	if err := m.ingestLocal(MethodStarted, ingestFrame(long, StateRunning)); err == nil {
		t.Error("a workloadInfo.id over the limit was accepted")
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"`+long+`"}`)); err == nil {
		t.Error("a workloadId over the limit was accepted")
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("an oversize id reached the broker: %s", out)
	}
}

func TestIngestLocalAcceptsIDAtLimit(t *testing.T) {
	m, _ := newTestManager(t)
	id := strings.Repeat("a", localIngressMaxIDLen)
	if err := m.ingestLocal(MethodStarted, ingestFrame(id, StateRunning)); err != nil {
		t.Fatalf("workloadInfo.id at the limit refused: %v", err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"`+id+`"}`)); err != nil {
		t.Fatalf("workloadId at the limit refused: %v", err)
	}
}

// The stdio path is not the ingress's business: what the ingress refuses is
// still tracked when the broker sends it.
func TestStdioLifecycleKeepsAcceptingWhatTheIngressRefuses(t *testing.T) {
	for name, params := range map[string]string{
		"unknown state":   `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"paused","originatedFrom":"self-uuid","createdAt":1}}`,
		"foreign origin":  `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"other-node","createdAt":1}}`,
		"resync marker":   `{"resync":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1}}`,
		"long identifier": `{"workloadInfo":{"id":"` + strings.Repeat("a", localIngressMaxIDLen+1) + `","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m.handleMessage(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: json.RawMessage(params)})
			if got := len(m.activeSnapshot()); got != 1 {
				t.Fatalf("re-sync set = %d, want 1", got)
			}
		})
	}
}

func TestNewLocalIngressRefusesNonLoopback(t *testing.T) {
	noop := func(string, json.RawMessage) error { return nil }
	for _, addr := range []string{"0.0.0.0:14324", ":14324", "192.0.2.5:14324", "[::]:14324", "example.com:14324", "nonsense"} {
		if _, err := newLocalIngress(addr, noop); err == nil {
			t.Errorf("%q accepted, want refusal", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0", "127.5.5.5:14324"} {
		if _, err := newLocalIngress(addr, noop); err != nil {
			t.Errorf("%q refused: %v", addr, err)
		}
	}
}

func TestLocalIngressEndToEnd(t *testing.T) {
	m, rw := newTestManager(t)
	li, err := newLocalIngress("127.0.0.1:0", m.ingestLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := li.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = li.Serve(ctx) }()
	url := "http://" + li.Addr() + eventsPath

	post := func(body string) int {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Origin left empty: stamped, tracked, emitted upward.
	if code := post(`{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"j9","model":"qwen","engine":"llamacpp","runId":"j9","state":"running","createdAt":1,"startedAt":1,"completedAt":null,"error":null,"requesterId":null}}}`); code != http.StatusOK {
		t.Fatalf("post = %d, want 200", code)
	}
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1", got)
	}
	if out := rw.String(); !strings.Contains(out, `"originatedFrom":"self-uuid"`) || !strings.Contains(out, `"method":"workloads:upsert"`) {
		t.Fatalf("upsert missing or origin not stamped: %s", out)
	}
	// Producer mistakes are 400s and never reach the broker or the wire.
	if code := post(`{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":""}}}`); code != http.StatusBadRequest {
		t.Fatalf("malformed = %d, want 400", code)
	}
	if code := post(`{"jsonrpc":"1.0","method":"workload:started","params":{}}`); code != http.StatusBadRequest {
		t.Fatalf("wrong version = %d, want 400", code)
	}
	if code := post(`{"jsonrpc":"2.0","method":"discovery:nodes","params":{}}`); code != http.StatusBadRequest {
		t.Fatalf("foreign method = %d, want 400", code)
	}
	if code := post(`not json`); code != http.StatusBadRequest {
		t.Fatalf("not json = %d, want 400", code)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", resp.StatusCode)
	}
	// Removal without an origin is stamped too and clears the re-sync set.
	if code := post(`{"jsonrpc":"2.0","method":"workloads:remove","params":{"workloadId":"j9"}}`); code != http.StatusOK {
		t.Fatalf("remove = %d, want 200", code)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
}

func TestResolveLocalIngressAddr(t *testing.T) {
	dir := t.TempDir()
	cluster := filepath.Join(dir, "cluster")
	if got := resolveLocalIngressAddr("127.0.0.1:1", cluster); got != "127.0.0.1:1" {
		t.Fatalf("flag not honoured: %q", got)
	}
	if got := resolveLocalIngressAddr("", cluster); got != "" {
		t.Fatalf("no file should mean off, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, localIngressConfigFile), []byte(`{"listen":" 127.0.0.1:14324 "}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveLocalIngressAddr("", cluster); got != "127.0.0.1:14324" {
		t.Fatalf("file not honoured: %q", got)
	}
	if got := resolveLocalIngressAddr("127.0.0.1:2", cluster); got != "127.0.0.1:2" {
		t.Fatalf("flag must win over the file: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, localIngressConfigFile), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveLocalIngressAddr("", cluster); got != "" {
		t.Fatalf("malformed file should mean off, got %q", got)
	}
}

func TestNewManagerRefusesNonLoopbackIngress(t *testing.T) {
	if _, err := NewManager(NewCodec(&captureRW{}), 0, "self", "", "0.0.0.0:14324"); err == nil {
		t.Fatal("a non-loopback ingress address must fail construction")
	}
}

// startIngress serves a loopback ingress over m and returns it with its events URL.
func startIngress(t *testing.T, m *Manager) (*localIngress, string) {
	t.Helper()
	li, err := newLocalIngress("127.0.0.1:0", m.ingestLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := li.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = li.Serve(ctx) }()
	return li, "http://" + li.Addr() + eventsPath
}

// sendIngress sends body with method as application/json, after mutate has
// adjusted the request, and returns the status code.
func sendIngress(t *testing.T, method, url, body string, mutate func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func postIngress(t *testing.T, url, body string, mutate func(*http.Request)) int {
	t.Helper()
	return sendIngress(t, http.MethodPost, url, body, mutate)
}

func TestLocalIngressRejectsNonJSONContentType(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data", ""} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) {
			if ct == "" {
				r.Header.Del("Content-Type")
			} else {
				r.Header.Set("Content-Type", ct)
			}
		})
		if code != http.StatusUnsupportedMediaType {
			t.Errorf("content type %q = %d, want 415", ct, code)
		}
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a rejected request reached the broker: %s", out)
	}
}

func TestLocalIngressAcceptsJSONContentTypeWithCharset(t *testing.T) {
	m, _ := newTestManager(t)
	_, url := startIngress(t, m)
	code := postIngress(t, url, startedBody("j1"), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	})
	if code != http.StatusOK {
		t.Fatalf("application/json with a charset = %d, want 200", code)
	}
}

func TestLocalIngressRejectsOriginHeader(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:14324", "null", ""} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) { r.Header.Set("Origin", origin) })
		if code != http.StatusForbidden {
			t.Errorf("Origin %q = %d, want 403", origin, code)
		}
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a rejected request reached the broker: %s", out)
	}
}

func TestLocalIngressRejectsNonLoopbackHost(t *testing.T) {
	m, rw := newTestManager(t)
	li, url := startIngress(t, m)
	_, port, _ := net.SplitHostPort(li.Addr())
	for _, host := range []string{"evil.example:" + port, "evil.example", "127.0.0.1", "127.0.0.1:1", "192.0.2.5:" + port, "0.0.0.0:" + port, "localhost.evil.example:" + port, "[::1]"} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) { r.Host = host })
		if code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q = %d, want 421", host, code)
		}
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a rejected request reached the broker: %s", out)
	}
}

func TestLocalIngressAcceptsLoopbackHostNames(t *testing.T) {
	m, _ := newTestManager(t)
	li, url := startIngress(t, m)
	_, port, _ := net.SplitHostPort(li.Addr())
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "LOCALHOST:" + port, "127.5.5.5:" + port, "[::1]:" + port} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) { r.Host = host })
		if code != http.StatusOK {
			t.Errorf("Host %q = %d, want 200", host, code)
		}
	}
}

// failReader fails the test if the handler reads the body.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("the request body was read before the header checks")
	return 0, io.EOF
}

func TestLocalIngressHeaderChecksPrecedeBodyRead(t *testing.T) {
	li, err := newLocalIngress("127.0.0.1:14324", func(string, json.RawMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*http.Request){
		"host":         func(r *http.Request) { r.Host = "evil.example" },
		"origin":       func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"content type": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, eventsPath, failReader{t})
			req.Host = "127.0.0.1:14324"
			req.Header.Set("Content-Type", "application/json")
			mutate(req)
			rec := httptest.NewRecorder()
			li.handle(rec, req)
			if rec.Code < 400 {
				t.Fatalf("status = %d, want a rejection", rec.Code)
			}
		})
	}
}

// A request that breaks several rules is answered by the first one in the
// documented order: method, Host, Origin, content type, then the frame.
func TestLocalIngressChecksRunInOrder(t *testing.T) {
	m, _ := newTestManager(t)
	li, url := startIngress(t, m)
	_, port, _ := net.SplitHostPort(li.Addr())
	badHost := func(r *http.Request) { r.Host = "evil.example:" + port }
	origin := func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }
	plain := func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }
	all := func(fns ...func(*http.Request)) func(*http.Request) {
		return func(r *http.Request) {
			for _, fn := range fns {
				fn(r)
			}
		}
	}
	for name, tc := range map[string]struct {
		method string
		mutate func(*http.Request)
		body   string
		want   int
	}{
		"method before host":         {http.MethodGet, all(badHost, origin, plain), startedBody("j1"), http.StatusMethodNotAllowed},
		"host before origin":         {http.MethodPost, all(badHost, origin, plain), startedBody("j1"), http.StatusMisdirectedRequest},
		"origin before content type": {http.MethodPost, all(origin, plain), startedBody("j1"), http.StatusForbidden},
		"content type before frame":  {http.MethodPost, plain, `not json`, http.StatusUnsupportedMediaType},
		"frame last":                 {http.MethodPost, nil, `not json`, http.StatusBadRequest},
	} {
		if code := sendIngress(t, tc.method, url, tc.body, tc.mutate); code != tc.want {
			t.Errorf("%s = %d, want %d", name, code, tc.want)
		}
	}
}

func TestLocalIngressRejectsBodyOverCap(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	// A valid frame padded with whitespace past the cap: truncating the read
	// would still parse it, so only a real limit rejects it.
	padded := startedBody("j1") + strings.Repeat(" ", localIngressBodyCap)
	if code := postIngress(t, url, padded, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body = %d, want 413", code)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("an oversize body reached the broker: %s", out)
	}
}

func TestLocalIngressAcceptsBodyAtCap(t *testing.T) {
	m, _ := newTestManager(t)
	_, url := startIngress(t, m)
	body := startedBody("j1")
	body += strings.Repeat(" ", localIngressBodyCap-len(body))
	if code := postIngress(t, url, body, nil); code != http.StatusOK {
		t.Fatalf("body of exactly the cap = %d, want 200", code)
	}
}

func TestLocalIngressServerBoundsReadsAndIdleConnections(t *testing.T) {
	li, err := newLocalIngress("127.0.0.1:0", func(string, json.RawMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	srv := li.newServer()
	if srv.ReadHeaderTimeout != interNodeReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, interNodeReadHeaderTimeout)
	}
	if srv.ReadTimeout != 10*time.Second {
		t.Errorf("ReadTimeout = %v, want 10s", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 60*time.Second {
		t.Errorf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
	}
}

// A WriteTimeout would cover the whole handler, so a slow broker write would
// lose the response after the frame had landed and the producer would retry it.
func TestLocalIngressServerSetsNoWriteTimeout(t *testing.T) {
	li, err := newLocalIngress("127.0.0.1:0", func(string, json.RawMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := li.newServer().WriteTimeout; got != 0 {
		t.Fatalf("WriteTimeout = %v, want none", got)
	}
}

func TestLocalIngressForeignOriginIs400(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	upsert := `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"other-node","createdAt":1}}}`
	if code := postIngress(t, url, upsert, nil); code != http.StatusBadRequest {
		t.Fatalf("foreign-origin upsert = %d, want 400", code)
	}
	remove := `{"jsonrpc":"2.0","method":"workloads:remove","params":{"workloadId":"W","originatedFrom":"other-node"}}`
	if code := postIngress(t, url, remove, nil); code != http.StatusBadRequest {
		t.Fatalf("foreign-origin remove = %d, want 400", code)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a foreign-origin frame reached the broker: %s", out)
	}
}

func TestLocalIngressAcceptsOwnOrigin(t *testing.T) {
	m, _ := newTestManager(t)
	_, url := startIngress(t, m)
	upsert := `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1}}}`
	if code := postIngress(t, url, upsert, nil); code != http.StatusOK {
		t.Fatalf("own-origin upsert = %d, want 200", code)
	}
	remove := `{"jsonrpc":"2.0","method":"workloads:remove","params":{"workloadId":"W","originatedFrom":"self-uuid"}}`
	if code := postIngress(t, url, remove, nil); code != http.StatusOK {
		t.Fatalf("own-origin remove = %d, want 200", code)
	}
}

func TestLocalIngressSpellingBypassesAre400(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	for name, tc := range spellingBypasses {
		if code := postIngress(t, url, rpcBody(tc.method, json.RawMessage(tc.params)), nil); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a refused frame reached the broker: %s", out)
	}
}

// The shape a real harness sends: canonical keys throughout, every workload field
// present, no origin of its own.
func TestLocalIngressAcceptsCanonicalHarnessFrame(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	frame := `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"job-42","model":"qwen3-32b","engine":"llamacpp","runId":"run-7","state":"running","originatedFrom":"","scheduledOn":"aorus","createdAt":1700000000000,"startedAt":1700000000100,"completedAt":null,"error":null,"requesterId":"scheduler"}}}`
	if code := postIngress(t, url, frame, nil); code != http.StatusOK {
		t.Fatalf("harness frame = %d, want 200", code)
	}
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1", got)
	}
	if out := rw.String(); !strings.Contains(out, `"id":"job-42"`) || !strings.Contains(out, `"originatedFrom":"self-uuid"`) {
		t.Fatalf("broker did not receive the stamped upsert: %s", out)
	}
}

func TestLocalIngressResyncMarkerIs400(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	body := `{"jsonrpc":"2.0","method":"workload:started","params":{"resync":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1}}}`
	if code := postIngress(t, url, body, nil); code != http.StatusBadRequest {
		t.Fatalf("resync marker = %d, want 400", code)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a frame carrying resync reached the broker: %s", out)
	}
}

func TestLocalIngressUnknownStateIs400(t *testing.T) {
	m, _ := newTestManager(t)
	_, url := startIngress(t, m)
	body := `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"done","createdAt":1}}}`
	if code := postIngress(t, url, body, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown state = %d, want 400", code)
	}
}

func TestLocalIngressAcceptsProducerStates(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	for method, state := range map[string]WorkloadState{
		MethodSubmitted: StateQueued,
		MethodStarted:   StateRunning,
		MethodCompleted: StateCompleted,
		MethodErrored:   StateFailed,
	} {
		if code := postIngress(t, url, rpcBody(method, ingestFrame(string(state), state)), nil); code != http.StatusOK {
			t.Errorf("state %q = %d, want 200", state, code)
		}
	}
	if got := strings.Count(rw.String(), `"method":"workloads:upsert"`); got != 4 {
		t.Fatalf("broker received %d upserts, want 4: %s", got, rw.String())
	}
}

func TestLocalIngressOversizeIDIs400(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	long := strings.Repeat("a", localIngressMaxIDLen+1)
	if code := postIngress(t, url, rpcBody(MethodStarted, ingestFrame(long, StateRunning)), nil); code != http.StatusBadRequest {
		t.Fatalf("workloadInfo.id over the limit = %d, want 400", code)
	}
	remove := rpcBody(MethodRemove, json.RawMessage(`{"workloadId":"`+long+`"}`))
	if code := postIngress(t, url, remove, nil); code != http.StatusBadRequest {
		t.Fatalf("workloadId over the limit = %d, want 400", code)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("an oversize id reached the broker: %s", out)
	}
}
