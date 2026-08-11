package orchestrator

// The outward-facing boundary's enforcement half (ADR-0007): the §6.1 listener is
// refused anywhere but loopback unless the deployment says otherwise out loud.
//
// Authentication and TLS live OUTSIDE the engine — §1.4 excludes the credential
// family permanently, and §1.1 bars those names as engine-internal entities — so
// what the engine contributes to its own security posture is not a verifier but
// this refusal: with the listener pinned to loopback, whatever terminates TLS and
// authenticates in front of it is the only outward-facing surface BY
// CONSTRUCTION, rather than by an operator remembering it. A default is not a
// boundary; `--addr` has defaulted to 127.0.0.1:8080 all along, and an operator who
// types a routable address for one test publishes an unauthenticated control plane
// that can create root-capable sandboxes.

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ErrListenAddrNotLoopback is the STARTUP refusal of a §6.1 listen address that is
// not loopback, absent an explicit override.
//
// Deliberately not a per-request error, for the reason ErrSecondHopUIDIsTheEngineItself
// gives about its own case: this is a property of the DEPLOYMENT alone. It is
// decidable with no request in hand, it must be decided before anything is served,
// and no request ever reaches a state where reporting it would be right.
var ErrListenAddrNotLoopback = errors.New("orchestrator: the §6.1 listen address is not loopback")

// ResolveHost maps a hostname to its addresses. nil → net.LookupHost.
//
// Injectable for the same reason network.Deployment.HostLocal is: it makes the
// name arm's assertion pinnable in a root-free unit test against a fixture, rather
// than against whatever this machine's /etc/hosts and resolver happen to say.
type ResolveHost func(host string) ([]string, error)

// ValidateListenAddr decides whether addr may be served without an explicit
// insecure-mode override (ADR-0007).
//
// It answers on the PARSED address rather than on the string, because the string
// forms that need catching are exactly the ones a prefix comparison gets wrong.
// Measured, against whether a listener on each spelling is reachable from this
// host's routable address:
//
//	127.0.0.1:8080            IsLoopback=true   binds 127.0.0.1:8080     loopback only
//	127.0.0.2:8080            IsLoopback=true   binds 127.0.0.2:8080     loopback only
//	[::1]:8080                IsLoopback=true   binds [::1]:8080         loopback only
//	[::ffff:127.0.0.1]:8080   IsLoopback=true   binds 127.0.0.1:8080     loopback only
//	0.0.0.0:8080              IsLoopback=false  binds [::]:8080          REACHABLE
//	[::]:8080                 IsLoopback=false  binds [::]:8080          REACHABLE
//	192.168.139.93:8080       IsLoopback=false  binds …:8080             REACHABLE
//
// netip's verdict matches reachability on every IP literal, the v4-mapped form
// included — which is the one a comparison against "127." reports backwards.
//
// The two spellings that are not literals get their own arms, and neither may be
// waved through:
//
//   - AN EMPTY HOST (":8080") binds every interface and is measured reachable, so it
//     is refused. It reads like "the default" and is the opposite of one.
//   - A NAME ("localhost:8080") resolves through configuration the DEPLOYMENT
//     controls, so "localhost is loopback" is not a fact the engine may assume. It
//     is accepted only when EVERY resolved address is loopback: accepting on "any"
//     would let a single /etc/hosts line mapping one name to both 127.0.0.1 and a
//     routable address open the wall while this check passes.
func ValidateListenAddr(addr string, resolve ResolveHost) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not addr's loopback-ness that is wrong — it has no decidable host at all,
		// and net.Listen would reject it too. Refusing here means the diagnostic
		// names the flag instead of surfacing as a bare listen error at the end of
		// the boot sequence, after the second-hop table is already in the kernel.
		return fmt.Errorf("orchestrator: --addr %q is not a host:port address: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("%w: %q has an EMPTY host, which binds every interface on this machine "+
			"(measured: it binds [::]:%s and is reachable from this host's routable address). Write "+
			"the address you mean — \"127.0.0.1:%s\" for the loopback listener this engine expects, "+
			"or pass --insecure-listen to publish the API deliberately. %s",
			ErrListenAddrNotLoopback, addr, port, port, whyLoopback)
	}

	if ip, perr := netip.ParseAddr(host); perr == nil {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%w: %q is a routable address. %s",
			ErrListenAddrNotLoopback, addr, whyLoopback)
	}

	// A name. Resolve it and require the WHOLE answer to be loopback.
	if resolve == nil {
		resolve = net.LookupHost
	}
	addrs, rerr := resolve(host)
	if rerr != nil {
		return fmt.Errorf("%w: %q does not resolve (%v), so whether it is loopback cannot be "+
			"decided — and this check must not pass an address it could not classify",
			ErrListenAddrNotLoopback, addr, rerr)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %q resolved to no addresses, so whether it is loopback cannot be decided",
			ErrListenAddrNotLoopback, addr)
	}
	var routable []string
	for _, a := range addrs {
		ip, perr := netip.ParseAddr(a)
		if perr != nil {
			// An answer this code cannot parse is an answer it cannot clear.
			routable = append(routable, a)
			continue
		}
		if !ip.IsLoopback() {
			routable = append(routable, ip.String())
		}
	}
	if len(routable) > 0 {
		return fmt.Errorf("%w: the name in %q resolves to %s, which %s not loopback. Every address a "+
			"name resolves to must be loopback — one routable answer is enough to publish the API, so "+
			"clearing the name on the strength of its loopback answers would open the wall while this "+
			"check passed. %s",
			ErrListenAddrNotLoopback, addr, strings.Join(routable, ", "), plural(len(routable)), whyLoopback)
	}
	return nil
}

// whyLoopback is the tail shared by every refusal above: the operator needs the
// consequence and the way out, not just the verdict.
const whyLoopback = "The §6.1 API has NO authentication and NO TLS — by design, since the " +
	"credential family is permanently excluded from the engine (§1.4) — so publishing this listener " +
	"exposes an unauthenticated control plane that can create root-capable sandboxes to anything that " +
	"can route to this host. Put a TLS-terminating, authenticating gateway in front of it and leave " +
	"this listener on loopback (ADR-0007), or pass --insecure-listen if this deployment is reachable " +
	"only by authorized callers for some reason the engine cannot see."

func plural(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
