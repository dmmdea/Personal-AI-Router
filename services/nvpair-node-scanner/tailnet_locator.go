// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/tailnet"
)

// The tailnet locator finds cluster members that multicast cannot reach.
//
// mDNS is link-local: a member that shares only a tailnet with this node (a laptop
// away from home, a box in another building) is never heard, so the directory —
// and with it every channel that takes its peers from the directory: engine
// status, the Jobs list, error sync, roster reconcile — loses it, in both
// directions. The local Tailscale client already knows every device on the
// tailnet and the overlay address it answers on; what it cannot say is which of
// them are members of THIS cluster. The locator settles that the only way the
// cluster accepts: a TLS handshake against the peer's engine-manager port that
// presents our leaf and accepts the server's certificate only when it is pinned,
// byte for byte, as a member (Mesh.ClientTLSConfigAny). The UUID on that
// certificate — not a hostname, not a node-info answer, not anything the
// tailnet reports — is what an address is filed under.
//
// So the locator adds reach, never trust: an address it reports is a place a
// pinned member proved it is listening, and every channel that later dials it
// still pins the certificate itself.

const (
	// tailnetRefreshInterval paces Tailscale status reads and member proofs.
	tailnetRefreshInterval = 30 * time.Second
	// tailnetProveTimeout bounds one connect + handshake with a device that has
	// proved to be a member before. A first packet over a relayed (DERP) path can
	// take a second or more, so it is longer than the LAN-tuned
	// reach.DefaultTimeout.
	tailnetProveTimeout = 4 * time.Second
	// tailnetFirstProveTimeout bounds the same for a device never proven. Most of
	// those are not members at all, and a policy that drops their packets
	// silently would otherwise hold a refresh for the full member budget.
	tailnetFirstProveTimeout = 2500 * time.Millisecond
	// tailnetStaleAfter is how long a member's last proof still vouches for it:
	// it bridges one missed proof (a lost packet on a relayed path, a restart, a
	// CLI hiccup) without dropping anyone, and bounds how long a member that
	// really went away is kept.
	tailnetStaleAfter = 2 * tailnetRefreshInterval
	// tailnetMemberWindow is how long a device stays "a member" for retry
	// purposes after its last proof: within it a failed proof is retried on the
	// next refresh and the device is asked even while the control plane calls it
	// offline; past it the device backs off like any other.
	tailnetMemberWindow = 10 * time.Minute
	// tailnetRejectTTL parks a device that answered with a certificate that is
	// not a pinned member: a definite "not one of ours". Only a change in the
	// cluster's pins lifts it early.
	tailnetRejectTTL = 10 * time.Minute
	// tailnetBackoffMax caps the backoff for a device that did not answer at all
	// (refused, timed out). That is not proof of anything, so it backs off from
	// one refresh interval rather than being parked outright.
	tailnetBackoffMax = 10 * time.Minute
	// tailnetProbeConcurrency bounds simultaneous handshakes in one refresh.
	tailnetProbeConcurrency = 4
	// tailnetMaxFaults is how many consecutive faulted refreshes switch the
	// locator off for the life of the process.
	tailnetMaxFaults = 3
	// tailnetDisableEnv turns the locator off ("0", "false", "off", "no").
	tailnetDisableEnv = "NVPAIR_TAILNET_DISCOVERY"
)

// tailnetQuickRetries are the delays used instead of tailnetRefreshInterval
// after a network change, or while a located member keeps missing its proof —
// the moments when the next answer is most likely to differ and most valuable
// to have early (a laptop that just woke on another network).
var tailnetQuickRetries = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second}

// tailnetDisabled reports whether the operator switched the locator off.
func tailnetDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(tailnetDisableEnv))) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// tailnetHint is one member located over the tailnet.
type tailnetHint struct {
	uuid     string
	ip       string
	hostName string // the device's OS hostname per Tailscale (PAIR's instance name)
}

type tailnetVerified struct {
	uuid     string
	hostName string
	at       time.Time
	epoch    uint64 // the refresh that recorded this proof
}

type tailnetBackoff struct {
	until      time.Time
	fails      int
	definitive bool // answered with a certificate that is not a member's
}

type tailnetCandidate struct{ ip, hostName string }

type tailnetLocator struct {
	status    func(ctx context.Context) (tailnet.Snapshot, error)
	tlsConfig func() (*tls.Config, bool)
	selfUUID  func() string
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	emPort    int
	now       func() time.Time

	kick        chan struct{}
	probeFaults atomic.Int64 // probes that panicked, counted toward tailnetMaxFaults

	mu        sync.Mutex
	verified  map[string]tailnetVerified // overlay IP -> last successful proof
	backoff   map[string]tailnetBackoff  // overlay IP -> not asked again before
	hints     map[string]tailnetHint     // member UUID -> proven in the last refresh
	lastErr   string
	logged    bool   // the "no Tailscale client" notice is logged once
	clearAll  bool   // a pending kick lifts definitive parks too (pins changed)
	clearSome bool   // a pending kick lifts the backoff of silent devices (network moved)
	epoch     uint64 // counts refreshes; a hint is a proof from the current one
}

// newTailnetLocator wires the locator to the local Tailscale CLI and the
// cluster's pins.
func newTailnetLocator(mesh *clustertrust.Mesh, selfUUID func() string) *tailnetLocator {
	// Resolved lazily and again while absent, so a Tailscale client installed
	// after PAIR started is picked up without a restart. Only the locator's own
	// goroutine calls status.
	var run tailnet.Runner
	dialer := &net.Dialer{}
	return &tailnetLocator{
		status: func(ctx context.Context) (tailnet.Snapshot, error) {
			if run == nil {
				run = tailnet.CLIRunner()
			}
			return tailnet.Status(ctx, run)
		},
		tlsConfig: func() (*tls.Config, bool) {
			mesh.Refresh()
			return mesh.ClientTLSConfigAny()
		},
		selfUUID: selfUUID,
		dial:     dialer.DialContext,
		emPort:   defaultEngineManagerPort,
		now:      time.Now,
		kick:     make(chan struct{}, 1),
		verified: make(map[string]tailnetVerified),
		backoff:  make(map[string]tailnetBackoff),
		hints:    make(map[string]tailnetHint),
	}
}

// run refreshes on a timer, sooner after a kick (a local network change, a
// change in the cluster's pins) or while a located member is missing its proof.
//
// The scanner is the one worker the broker cannot run without, so the locator
// must never take it down: a refresh or probe that panics is recovered and
// counted, and a run of them switches the locator off — discovery then carries
// on over mDNS exactly as it did before the locator existed.
func (l *tailnetLocator) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	quick, faults := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-l.kick:
			l.applyKick()
			quick = 0
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		retry, faulted := l.safeRefresh(ctx)
		if faulted {
			faults++
			if faults >= tailnetMaxFaults {
				slog.Error("tailnet discovery switched off after repeated faults; mDNS discovery continues", "faults", faults)
				l.replaceHints(nil)
				return
			}
		} else {
			faults = 0
		}
		// Quick retries run out after the last entry and the cadence settles at
		// the normal interval until a refresh comes back clean (or another kick).
		next := tailnetRefreshInterval
		switch {
		case !retry:
			quick = 0
		case quick < len(tailnetQuickRetries):
			next = tailnetQuickRetries[quick]
			quick++
		}
		timer.Reset(next)
	}
}

// safeRefresh is refresh with any fault — in refresh itself or in one of its
// probes — turned into a logged, counted failure.
func (l *tailnetLocator) safeRefresh(ctx context.Context) (retry, faulted bool) {
	before := l.probeFaults.Load()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("tailnet discovery refresh failed", "panic", r)
			retry, faulted = true, true
		}
	}()
	retry = l.refresh(ctx)
	return retry, l.probeFaults.Load() != before
}

// poke requests a refresh after a local network change: devices that did not
// answer are asked again, devices that proved not to be members stay parked.
// Safe on a nil locator.
func (l *tailnetLocator) poke() { l.request(false) }

// pokeTrust requests a refresh after the cluster's pins changed: a device set
// aside as "not a member" may be one now, so every backoff is lifted. Safe on a
// nil locator.
func (l *tailnetLocator) pokeTrust() { l.request(true) }

func (l *tailnetLocator) request(all bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if all {
		l.clearAll = true
	} else {
		l.clearSome = true
	}
	l.mu.Unlock()
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// applyKick lifts the backoffs the pending kicks asked for.
func (l *tailnetLocator) applyKick() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.backoff {
		if l.clearAll || (l.clearSome && !b.definitive) {
			delete(l.backoff, ip)
		}
	}
	l.clearAll, l.clearSome = false, false
}

// current returns a copy of the members proven in the last refresh, by UUID.
func (l *tailnetLocator) current() map[string]tailnetHint {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]tailnetHint, len(l.hints))
	for k, v := range l.hints {
		out[k] = v
	}
	return out
}

// vouches reports whether member uuid proved itself with its pinned certificate
// within tailnetStaleAfter. Safe on a nil locator.
func (l *tailnetLocator) vouches(uuid string) bool {
	_, ok := l.lastProven(uuid)
	return ok
}

// addressesFor returns the overlay address member uuid was last proven at, while
// that proof still vouches for it. Safe on a nil locator.
func (l *tailnetLocator) addressesFor(uuid string) []string {
	if ip, ok := l.lastProven(uuid); ok {
		return []string{ip}
	}
	return nil
}

// lastProven returns the address of uuid's most recent proof within
// tailnetStaleAfter; the current hint wins when there is one.
func (l *tailnetLocator) lastProven(uuid string) (string, bool) {
	if l == nil || uuid == "" {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if h, ok := l.hints[uuid]; ok {
		return h.ip, true
	}
	now := l.now()
	best, bestAt := "", time.Time{}
	for ip, v := range l.verified {
		if v.uuid == uuid && now.Sub(v.at) < tailnetStaleAfter && v.at.After(bestAt) {
			best, bestAt = ip, v.at
		}
	}
	return best, best != ""
}

// provenAddress reports whether ip is an overlay address some member proved
// itself at. Safe on a nil locator.
func (l *tailnetLocator) provenAddress(ip string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.verified[ip]
	return ok
}

// refresh reads the tailnet once, proves every candidate it is not backing off
// from — devices that proved to be members recently first, published as soon as
// they answer, then the rest — and replaces the hint set. It reports whether a
// quick retry is worthwhile: the status could not be read, the tailnet is not
// running yet, or a located member missed its proof.
func (l *tailnetLocator) refresh(ctx context.Context) (retry bool) {
	// Without cluster membership there is nobody to find; do not even run the CLI.
	if _, clustered := l.tlsConfig(); !clustered {
		l.replaceHints(nil)
		return false
	}
	snap, err := l.status(ctx)
	if err != nil {
		l.noteStatusError(err)
		// Keep what was proven recently: a CLI hiccup says nothing about whether
		// the tailnet still carries traffic.
		l.expireHints()
		return !errors.Is(err, tailnet.ErrUnavailable)
	}
	l.noteStatusError(nil)
	if !snap.Running {
		// Stopped, logged out, or still starting after a wake: nothing is
		// reachable now, but "starting" resolves in seconds, so look again soon.
		l.replaceHints(nil)
		return true
	}

	cands, members, others, epoch := l.plan(snap)

	// Members first, and published at once, so a slow sweep of strangers never
	// holds a known member back.
	memberRetry := l.record(l.proveAll(ctx, members, tailnetProveTimeout), epoch)
	l.replaceHints(l.hintsFrom(cands, epoch))
	l.record(l.proveAll(ctx, others, tailnetFirstProveTimeout), epoch)
	l.replaceHints(l.hintsFrom(cands, epoch))
	return memberRetry
}

// plan opens a refresh: it picks the candidates from a snapshot, splits the ones
// not backing off into recent members and everyone else, and returns the epoch
// the refresh's proofs are recorded under.
func (l *tailnetLocator) plan(snap tailnet.Snapshot) (cands, members, others []tailnetCandidate, epoch uint64) {
	self := make(map[string]bool, len(snap.SelfIPv4))
	for _, ip := range snap.SelfIPv4 {
		self[ip] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.epoch++
	epoch = l.epoch
	now := l.now()
	present := make(map[string]bool)
	for _, p := range snap.Peers {
		// A device shared in from another tailnet belongs to someone else's
		// network and is never asked; an expired one cannot carry traffic.
		if p.SharedIn || p.Expired {
			continue
		}
		for _, ip := range p.IPv4 {
			if self[ip] || present[ip] {
				continue
			}
			// Online is the control plane's view, which goes stale exactly when
			// this node's own connection degrades. Devices it calls offline are
			// skipped unless they proved to be members recently.
			if !p.Online && !l.recentMemberLocked(ip, now) {
				continue
			}
			present[ip] = true
			cands = append(cands, tailnetCandidate{ip: ip, hostName: p.HostName})
		}
	}
	for ip := range l.verified {
		if !present[ip] {
			delete(l.verified, ip)
		}
	}
	for ip := range l.backoff {
		if !present[ip] {
			delete(l.backoff, ip)
		}
	}
	// Every candidate is proven on every refresh, members included: a located
	// member is reported to the browser as seen on each scan, so the proof is
	// also its liveness. Only devices that are backing off are skipped.
	for _, c := range cands {
		if b, ok := l.backoff[c.ip]; ok && now.Before(b.until) {
			continue
		}
		if l.recentMemberLocked(c.ip, now) {
			members = append(members, c)
		} else {
			others = append(others, c)
		}
	}
	return cands, members, others, epoch
}

// recentMemberLocked reports whether ip proved to be a member within
// tailnetMemberWindow. l.mu must be held.
func (l *tailnetLocator) recentMemberLocked(ip string, now time.Time) bool {
	v, ok := l.verified[ip]
	return ok && now.Sub(v.at) < tailnetMemberWindow
}

type tailnetProof struct {
	c          tailnetCandidate
	uuid       string
	at         time.Time
	definitive bool
	ok         bool
}

// proveAll proves each candidate, tailnetProbeConcurrency at a time.
func (l *tailnetLocator) proveAll(ctx context.Context, cands []tailnetCandidate, timeout time.Duration) []tailnetProof {
	results := make([]tailnetProof, len(cands))
	slots := make(chan struct{}, tailnetProbeConcurrency)
	var wg sync.WaitGroup
	for i, c := range cands {
		results[i] = tailnetProof{c: c}
		wg.Add(1)
		slots <- struct{}{}
		go func(i int, c tailnetCandidate) {
			defer wg.Done()
			defer func() { <-slots }()
			// A probe parses TLS from any device on the tailnet; a fault there is
			// one failed probe, never the scanner.
			defer func() {
				if r := recover(); r != nil {
					slog.Error("tailnet probe failed", "ip", c.ip, "panic", r)
					l.probeFaults.Add(1)
				}
			}()
			uuid, definitive, ok := l.prove(ctx, c.ip, timeout)
			results[i] = tailnetProof{c: c, uuid: uuid, at: l.now(), definitive: definitive, ok: ok}
		}(i, c)
	}
	wg.Wait()
	return results
}

// record folds proof results into the verified and backoff state and reports
// whether a recent member missed its proof.
func (l *tailnetLocator) record(results []tailnetProof, epoch uint64) (memberMissed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, r := range results {
		switch {
		case r.ok:
			l.verified[r.c.ip] = tailnetVerified{uuid: r.uuid, hostName: r.c.hostName, at: r.at, epoch: epoch}
			delete(l.backoff, r.c.ip)
		case r.definitive:
			l.backoff[r.c.ip] = tailnetBackoff{until: now.Add(tailnetRejectTTL), definitive: true}
		case l.recentMemberLocked(r.c.ip, now):
			// A recent member that missed a proof is asked again soon, never parked.
			memberMissed = true
		default:
			b := l.backoff[r.c.ip]
			b.fails++
			b.definitive = false
			wait := tailnetRefreshInterval << min(b.fails-1, 5)
			b.until = now.Add(min(wait, tailnetBackoffMax))
			l.backoff[r.c.ip] = b
		}
	}
	return memberMissed
}

// hintsFrom builds the hint set from the members proven in this refresh: one
// address per member, chosen deterministically — candidates are in the
// snapshot's sorted peer order, and the first proven address wins.
func (l *tailnetLocator) hintsFrom(cands []tailnetCandidate, epoch uint64) map[string]tailnetHint {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := make(map[string]tailnetHint)
	for _, c := range cands {
		v, ok := l.verified[c.ip]
		if !ok || v.epoch != epoch {
			continue // not proven in this refresh
		}
		if _, taken := next[v.uuid]; taken {
			continue
		}
		next[v.uuid] = tailnetHint{uuid: v.uuid, ip: c.ip, hostName: v.hostName}
	}
	return next
}

// prove completes a pinned TLS handshake with whatever listens on ip's
// engine-manager port and returns the member UUID its certificate carries.
// definitive reports a failure that settles the question — the other side
// answered with a certificate that is not a pinned member, or it is this node —
// as opposed to one that says nothing (no answer, a timeout, a reset).
func (l *tailnetLocator) prove(ctx context.Context, ip string, timeout time.Duration) (uuid string, definitive, ok bool) {
	cfg, clustered := l.tlsConfig()
	if !clustered {
		return "", false, false
	}
	cfg = cfg.Clone()
	notMember := false
	if verify := cfg.VerifyPeerCertificate; verify != nil {
		cfg.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
			err := verify(raw, chains)
			notMember = err != nil
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := l.dial(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(l.emPort)))
	if err != nil {
		return "", false, false
	}
	conn := tls.Client(raw, cfg)
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// VerifyPeerCertificate (the pinned-DER check) runs before this side sends
	// its own certificate, so a non-member at the address never sees our leaf.
	if err := conn.HandshakeContext(ctx); err != nil {
		return "", notMember, false
	}
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", true, false
	}
	uuid = clustertrust.UUIDFromCert(certs[0])
	if uuid == "" || uuid == l.selfUUID() {
		return "", true, false
	}
	return uuid, false, true
}

// expireHints keeps only the hints whose proof still vouches for them.
func (l *tailnetLocator) expireHints() {
	l.replaceHints(l.vouchedHints())
}

func (l *tailnetLocator) vouchedHints() map[string]tailnetHint {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	next := make(map[string]tailnetHint, len(l.hints))
	for uuid, h := range l.hints {
		if v, ok := l.verified[h.ip]; ok && v.uuid == uuid && now.Sub(v.at) < tailnetStaleAfter {
			next[uuid] = h
		}
	}
	return next
}

// replaceHints installs a new hint set and logs what changed.
func (l *tailnetLocator) replaceHints(next map[string]tailnetHint) {
	if next == nil {
		next = make(map[string]tailnetHint)
	}
	prev := l.swapHints(next)

	var uuids []string
	for uuid := range prev {
		uuids = append(uuids, uuid)
	}
	for uuid := range next {
		if _, ok := prev[uuid]; !ok {
			uuids = append(uuids, uuid)
		}
	}
	sort.Strings(uuids)
	for _, uuid := range uuids {
		was, had := prev[uuid]
		now, has := next[uuid]
		switch {
		case has && (!had || was.ip != now.ip):
			slog.Info("cluster member located over the tailnet", "host_uuid", uuid, "ip", now.ip, "host", now.hostName)
		case had && !has:
			slog.Info("cluster member no longer located over the tailnet", "host_uuid", uuid, "ip", was.ip)
		}
	}
}

func (l *tailnetLocator) swapHints(next map[string]tailnetHint) map[string]tailnetHint {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := l.hints
	l.hints = next
	return prev
}

// noteStatusError logs Tailscale status failures once per distinct message, and
// the absence of a client once per process.
func (l *tailnetLocator) noteStatusError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		if l.lastErr != "" {
			slog.Info("tailnet status readable again")
		}
		l.lastErr = ""
		return
	}
	if errors.Is(err, tailnet.ErrUnavailable) {
		if !l.logged {
			slog.Info("tailnet discovery inactive: no Tailscale client found on this host")
			l.logged = true
		}
		return
	}
	if msg := err.Error(); msg != l.lastErr {
		slog.Warn("tailnet status unreadable; keeping recently proven members", "err", err)
		l.lastErr = msg
	}
}
