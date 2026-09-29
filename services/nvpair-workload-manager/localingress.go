// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode"

	"nvpair-shared/appdir"
)

// localIngressConfigFile is the app-data file that turns the local ingress on
// when the broker launches this worker without --local-ingress:
//
//	<appdir>/workload-ingress.json   {"listen": "127.0.0.1:14324"}
//
// It is file-registered the way an engine manifest is under <appdir>/engines/,
// so a third-party producer or an operator can enable it without rebuilding
// the desktop application. Absent, empty or malformed means off.
const localIngressConfigFile = "workload-ingress.json"

// Ingress limits. The ingress serves a handful of local producers, so the
// bounds are tight: a client that trickles a request must not pin a goroutine
// indefinitely, and an id is an opaque producer string that is copied into the
// re-sync set, every peer frame and the Jobs list, so it cannot be unbounded.
const (
	localIngressBodyCap     = 1 << 20
	localIngressReadTimeout = 10 * time.Second
	localIngressIdleTimeout = 60 * time.Second
	localIngressMaxIDLen    = 256
)

// errBrokerWrite marks an ingest failure on the UPWARD write to the broker (as
// opposed to a validation failure, which is the producer's fault). The local
// interface treats a severed broker as the shutdown signal; the ingress just
// reports 500 so the producer can retry after the restart.
var errBrokerWrite = errors.New("broker write failed")

// errIngressOrigin is a producer naming a node other than this one as the
// origin of a frame. The ingress reports workloads that run here; a frame for
// another node's workload would be re-asserted to peers as this node's own.
var errIngressOrigin = errors.New("originatedFrom must be empty or this node's UUID")

// errIngressResync is a producer sending the peers' anti-entropy marker. A
// re-assertion is this node's own act (Manager.broadcastResync); a producer's
// frame carrying it would bypass the receivers' dedup.
var errIngressResync = errors.New(`"resync" is reserved for peer re-assertions and is not accepted from a producer`)

// ingressParamFields and ingressWorkloadFields are the JSON keys the frame
// decoders read: the params envelope (lifecycleParams, removeParams) and the
// Workload inside it. They come from the struct tags, so this file never
// duplicates the Workload schema and a field added there is covered here.
var (
	ingressParamFields    = append(jsonFieldNames(reflect.TypeOf(lifecycleParams{})), jsonFieldNames(reflect.TypeOf(removeParams{}))...)
	ingressWorkloadFields = jsonFieldNames(reflect.TypeOf(Workload{}))
)

// localIngress is the optional plaintext loopback listener for third-party
// workload producers on THIS machine: an external scheduler, a local inference
// harness that routes around the proxies, anything that runs work PAIR should
// count and show. It shares the peer port's frame format and validation but
// not its trust model — it never leaves loopback and is off unless configured.
//
// A frame accepted here is treated as local origin: tracked for re-sync,
// broadcast to pinned peers, and emitted UP to the broker as the translated
// workloads:upsert / workloads:remove, exactly as if one of the proxies had
// produced it. That upward emission is the difference from the stdio path
// (whose frames the broker has already applied before forwarding them here).
//
// Loopback is not a trust boundary against a browser: a web page can POST to
// 127.0.0.1, and a rebinding page can reach it under its own hostname. The
// handler therefore refuses anything a browser could send (an Origin header, a
// non-JSON content type, a non-loopback Host) before it reads the body, and the
// frame itself is vetted as an untrusted producer's (parseIngressFrame).
type localIngress struct {
	addr   string
	ingest func(method string, params json.RawMessage) error
	ln     net.Listener
	srv    *http.Server
}

// newLocalIngress validates the bind address. Anything that is not loopback is
// refused: a network-reachable plaintext ingress would let any host on the LAN
// forge this node's workloads, which is precisely what the peer port's cluster
// mTLS exists to prevent.
func newLocalIngress(addr string, ingest func(string, json.RawMessage) error) (*localIngress, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("local ingress address %q: %w", addr, err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("local ingress address %q is not loopback; refusing to bind", addr)
	}
	return &localIngress{addr: addr, ingest: ingest}, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Listen binds the address. Kept separate from Serve so a caller (and the
// tests) can learn the bound port before serving, and so a bind failure is
// reported synchronously at startup rather than from a goroutine.
func (li *localIngress) Listen() error {
	ln, err := net.Listen("tcp", li.addr)
	if err != nil {
		return fmt.Errorf("local ingress listen on %s: %w", li.addr, err)
	}
	li.ln = ln
	return nil
}

// Addr is the bound address once Listen has run, else the configured one.
func (li *localIngress) Addr() string {
	if li.ln == nil {
		return li.addr
	}
	return li.ln.Addr().String()
}

// newServer builds the ingress server. It sets no WriteTimeout: that deadline
// covers the whole handler, so a slow broker write would lose the response
// after the frame had already landed, and the producer would retry it.
func (li *localIngress) newServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, li.handle)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: interNodeReadHeaderTimeout,
		ReadTimeout:       localIngressReadTimeout,
		IdleTimeout:       localIngressIdleTimeout,
	}
}

// Serve blocks until ctx is cancelled. Listen must have been called first.
func (li *localIngress) Serve(ctx context.Context) error {
	li.srv = li.newServer()
	errCh := make(chan error, 1)
	go func() {
		slog.Info("local workload ingress listening (loopback, plaintext)", "addr", li.Addr())
		if err := li.srv.Serve(li.ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = li.srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// hostAllowed reports whether a request's Host names this listener: a loopback
// name or IP literal carrying the bound port. A DNS-rebinding page arrives with
// its own hostname in Host, so anything else is refused.
func (li *localIngress) hostAllowed(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	_, boundPort, err := net.SplitHostPort(li.Addr())
	return err == nil && port == boundPort && isLoopbackHost(host)
}

// handle checks the request's headers before it reads a byte of the body:
// 405 for a method other than POST, 421 for a Host that is not this loopback
// listener, 403 for any Origin header (no legitimate producer is a browser page),
// and 415 for a content type other than application/json (which also keeps a
// cross-origin "simple" request from delivering a body at all). A body over
// localIngressBodyCap is 413; a frame the producer got wrong is 400.
func (li *localIngress) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !li.hostAllowed(r.Host) {
		li.reject(w, http.StatusMisdirectedRequest, "host must be a loopback name or address with the ingress port")
		return
	}
	if _, ok := r.Header["Origin"]; ok {
		li.reject(w, http.StatusForbidden, "requests carrying an Origin header are not accepted")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		li.reject(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, localIngressBodyCap))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			li.reject(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		li.badRequest(w, "read body: "+err.Error())
		return
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		li.badRequest(w, "invalid JSON: "+err.Error())
		return
	}
	if msg.JSONRPC != "2.0" {
		li.badRequest(w, "unsupported jsonrpc version")
		return
	}
	if err := li.ingest(msg.Method, msg.Params); err != nil {
		if errors.Is(err, errBrokerWrite) {
			slog.Error("local ingress could not reach the broker", "method", msg.Method, "err", err)
			http.Error(w, "broker unavailable", http.StatusInternalServerError)
			return
		}
		li.badRequest(w, err.Error())
		return
	}
	slog.Info("accepted local-ingress workload frame", "method", msg.Method)
	w.WriteHeader(http.StatusOK)
}

func (li *localIngress) badRequest(w http.ResponseWriter, reason string) {
	li.reject(w, http.StatusBadRequest, reason)
}

func (li *localIngress) reject(w http.ResponseWriter, status int, reason string) {
	slog.Warn("local ingress request rejected", "status", status, "reason", reason)
	http.Error(w, reason, status)
}

// parseIngressFrame validates a frame from a local producer and returns the
// params to apply. It is the ingress's trust boundary, applied on top of the
// checks every local frame gets (parseLifecycle, parseRemove): the producer may
// only report workloads that run on this node, in a lifecycle state the rest of
// the system knows, under an id of sane length. The stdio path never comes
// through here. The decoded values are checked as well as the raw JSON, so a
// spelling the raw checks did not foresee still cannot reach the broker or the
// peers.
func parseIngressFrame(method string, params json.RawMessage, selfUUID string) (json.RawMessage, error) {
	params, err := normalizeIngressParams(method, params, selfUUID)
	if err != nil {
		return nil, err
	}
	if method == MethodRemove {
		workloadID, originatedFrom, err := parseRemove(params)
		if err != nil {
			return nil, err
		}
		if err := checkIngressRemoval(workloadID, originatedFrom, selfUUID); err != nil {
			return nil, err
		}
		return params, nil
	}
	wl, err := parseLifecycle(params)
	if err != nil {
		return nil, err
	}
	if err := checkIngressWorkload(wl, selfUUID); err != nil {
		return nil, err
	}
	return params, nil
}

// checkIngressWorkload holds a decoded lifecycle workload to the ingress rules.
func checkIngressWorkload(wl *Workload, selfUUID string) error {
	if len(wl.ID) > localIngressMaxIDLen {
		return fmt.Errorf("workloadInfo.id is longer than %d bytes", localIngressMaxIDLen)
	}
	if wl.OriginatedFrom != selfUUID {
		return errIngressOrigin
	}
	// Any state other than the two terminal ones is tracked as active with no
	// expiry, so a misspelt state would be re-asserted to peers forever.
	if !isWorkloadState(wl.State) {
		return fmt.Errorf("workloadInfo.state %q is not a known state", wl.State)
	}
	return nil
}

// checkIngressRemoval holds a decoded removal to the ingress rules.
func checkIngressRemoval(workloadID, originatedFrom, selfUUID string) error {
	if len(workloadID) > localIngressMaxIDLen {
		return fmt.Errorf("params.workloadId is longer than %d bytes", localIngressMaxIDLen)
	}
	if originatedFrom != selfUUID {
		return errIngressOrigin
	}
	return nil
}

// isWorkloadState is true for the lifecycle states this release defines. Keep it
// in step with the WorkloadState constants in workload.go.
func isWorkloadState(s WorkloadState) bool {
	switch s {
	case StateInitializing, StateQueued, StateRunning, StateCompleted, StateFailed:
		return true
	}
	return false
}

// normalizeIngressParams fills originatedFrom with this node's UUID when a local
// producer left it empty — the same courtesy the broker extends to the proxies —
// and rejects any other value. Lifecycle frames carry it inside
// params.workloadInfo; removals carry it at the top level. It refuses the
// top-level "resync" flag, the peers' own marker for an anti-entropy
// re-assertion, which would make a producer's frame bypass their dedup. The
// params are edited as generic JSON so this file never duplicates the Workload
// schema.
//
// encoding/json matches field names case-insensitively, so "originatedfrom" or
// "Resync" is read downstream as the real field while an exact-key lookup here
// never sees it. Every object is therefore refused if a key spells a field the
// decoders read in another case, or if two keys differ only by case.
func normalizeIngressParams(method string, params json.RawMessage, selfUUID string) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &top); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	for key := range top {
		if strings.EqualFold(key, "resync") {
			return nil, errIngressResync
		}
	}
	if err := checkIngressKeys(top, ingressParamFields); err != nil {
		return nil, err
	}
	self, err := json.Marshal(selfUUID)
	if err != nil {
		return nil, err
	}
	switch {
	case method == MethodRemove:
		if err := checkIngressOrigin(top["originatedFrom"], selfUUID); err != nil {
			return nil, err
		}
		top["originatedFrom"] = self
		return json.Marshal(top)
	case isLifecycleMethod(method):
		var info map[string]json.RawMessage
		if raw, ok := top["workloadInfo"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &info); err != nil {
				return nil, fmt.Errorf("invalid params.workloadInfo: %w", err)
			}
		}
		if info == nil {
			return nil, fmt.Errorf("missing params.workloadInfo")
		}
		if err := checkIngressKeys(info, ingressWorkloadFields); err != nil {
			return nil, fmt.Errorf("params.workloadInfo: %w", err)
		}
		if err := checkIngressOrigin(info["originatedFrom"], selfUUID); err != nil {
			return nil, err
		}
		info["originatedFrom"] = self
		infoRaw, err := json.Marshal(info)
		if err != nil {
			return nil, err
		}
		top["workloadInfo"] = infoRaw
		return json.Marshal(top)
	}
	return nil, fmt.Errorf("unknown method: %s", method)
}

// checkIngressOrigin accepts an absent, null or empty origin, or this node's.
func checkIngressOrigin(raw json.RawMessage, selfUUID string) error {
	if isEmptyJSONString(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s != selfUUID {
		return errIngressOrigin
	}
	return nil
}

// checkIngressKeys refuses an object holding a key that names one of fields in a
// different case, or two keys that differ only by case.
func checkIngressKeys(obj map[string]json.RawMessage, fields []string) error {
	canonical := make(map[string]string, len(fields))
	for _, field := range fields {
		canonical[foldKey(field)] = field
	}
	seen := make(map[string]string, len(obj))
	for key := range obj {
		folded := foldKey(key)
		if other, dup := seen[folded]; dup {
			return fmt.Errorf("keys %q and %q differ only by case", other, key)
		}
		seen[folded] = key
		if field, ok := canonical[folded]; ok && key != field {
			return fmt.Errorf("key %q must be spelled %q", key, field)
		}
	}
	return nil
}

// foldKey maps s to a representative that two strings share exactly when
// strings.EqualFold holds for them — the folding encoding/json applies when it
// matches a key to a field — so keys that differ only by case are found in one
// pass rather than by comparing every pair.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < least {
				least = f
			}
		}
		b.WriteRune(least)
	}
	return b.String()
}

// jsonFieldNames lists the JSON keys the decoder reads into the struct type t.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

// isEmptyJSONString is true for an absent field, JSON null, or "".
func isEmptyJSONString(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == ""
}

// resolveLocalIngressAddr picks the ingress bind address: the flag when set,
// else <appdir>/workload-ingress.json beside the cluster dir, else "" (off).
func resolveLocalIngressAddr(flagValue, clusterDir string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	base := ""
	if clusterDir != "" {
		base = filepath.Dir(clusterDir)
	} else if d, err := appdir.Dir(); err == nil {
		base = d
	}
	if base == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(base, localIngressConfigFile))
	if err != nil {
		return ""
	}
	var cfg struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		slog.Warn("ignoring malformed local ingress config", "file", localIngressConfigFile, "err", err)
		return ""
	}
	return strings.TrimSpace(cfg.Listen)
}
