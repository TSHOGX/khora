package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The classifier's verdict per spelling. Every row here was checked against GROUND
// TRUTH before being written down — a listener was opened on the spelling and dialed
// from this host's routable address — so these expectations are what the kernel does,
// not what netip is assumed to do. The measured table is in the recorded design.
//
// The v4-mapped row is the one that earns the parsed-address approach: a string
// comparison against "127." reports [::ffff:127.0.0.1] as routable, and it binds
// 127.0.0.1.
func TestValidateListenAddrClassifiesEverySpellingTheWayTheKernelBindsIt(t *testing.T) {
	for _, tc := range []struct {
		addr    string
		ok      bool
		because string
	}{
		{"127.0.0.1:8080", true, "the canonical loopback listener"},
		{"127.0.0.2:8080", true, "the whole 127/8 is loopback, not just .1 — measured: loopback only"},
		{"[::1]:8080", true, "v6 loopback"},
		{"[::ffff:127.0.0.1]:8080", true, "v4-mapped loopback: measured to bind 127.0.0.1:8080"},
		{"0.0.0.0:8080", false, "measured: binds [::]:8080, reachable from this host's routable address"},
		{"[::]:8080", false, "measured: binds [::]:8080, reachable"},
		{"192.168.139.93:8080", false, "a routable address"},
		{"10.0.0.5:80", false, "RFC1918 is routable — private is not loopback"},
		{":8080", false, "an EMPTY host binds every interface; it reads like a default and is the opposite"},
		{"8080", false, "not a host:port address at all"},
	} {
		// nil resolver: every row is an IP literal or a syntax error, so none may
		// reach the name arm. If one does, LookupHost on a non-name errors and the
		// row fails — which is the correct outcome, since it would mean the literal
		// arm stopped recognizing a literal.
		err := ValidateListenAddr(tc.addr, nil)
		if tc.ok && err != nil {
			t.Errorf("ValidateListenAddr(%q) refused a loopback address (%s): %v", tc.addr, tc.because, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ValidateListenAddr(%q) accepted a NON-loopback address (%s) — that publishes an "+
				"unauthenticated control plane able to create root-capable sandboxes (the recorded design)",
				tc.addr, tc.because)
		}
	}
}

// A NAME is cleared only when EVERY address it resolves to is loopback. The mixed
// row is the one that matters: a single /etc/hosts line mapping one name to both
// 127.0.0.1 and a routable address would publish the API, and a check that cleared
// the name on the strength of its loopback answer would pass while it happened.
//
// Fixture-driven rather than resolver-driven, so the assertion does not depend on
// this machine's /etc/hosts (which, measured, maps localhost to 127.0.0.1 and ::1 —
// but that is a deployment's choice to make differently, which is the whole point).
func TestValidateListenAddrRequiresEveryResolvedAddressToBeLoopback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  []string
		err     error
		ok      bool
		because string
	}{
		{"all-loopback", []string{"127.0.0.1", "::1"}, nil, true,
			"every answer is loopback"},
		{"mixed", []string{"127.0.0.1", "192.168.1.10"}, nil, false,
			"ONE routable answer is enough to publish the API"},
		{"routable-only", []string{"192.168.1.10"}, nil, false, "no loopback answer at all"},
		{"unparseable", []string{"not-an-address"}, nil, false,
			"an answer the check cannot parse is an answer it cannot clear"},
		{"empty", nil, nil, false, "no answers means the question was not decided"},
		{"lookup-fails", nil, errors.New("no such host"), false,
			"an undecidable name must not be waved through"},
	} {
		resolve := func(host string) ([]string, error) {
			if host != tc.name {
				t.Errorf("resolver asked for %q, want %q", host, tc.name)
			}
			return tc.answer, tc.err
		}
		err := ValidateListenAddr(tc.name+":8080", resolve)
		if tc.ok && err != nil {
			t.Errorf("ValidateListenAddr(%q → %v) refused it (%s): %v", tc.name, tc.answer, tc.because, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ValidateListenAddr(%q → %v, err=%v) accepted it — %s (the recorded design)",
				tc.name, tc.answer, tc.err, tc.because)
		}
	}
}

// Every refusal is wrapped in ErrListenAddrNotLoopback so a caller can tell it from any other
// assembly failure, and carries the CONSEQUENCE plus the way out: a bare "invalid address" sends
// an operator to change the value until it is accepted, which here means finding the one spelling
// that slips through rather than making a decision.
func TestTheRefusalNamesTheConsequenceAndTheWayOut(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.10:8080"} {
		err := ValidateListenAddr(addr, nil)
		if err == nil {
			t.Fatalf("ValidateListenAddr(%q) accepted a non-loopback address", addr)
		}
		if !errors.Is(err, ErrListenAddrNotLoopback) {
			t.Errorf("%q: refusal is not wrapped in ErrListenAddrNotLoopback, so a caller cannot "+
				"distinguish it: %v", addr, err)
		}
		for _, want := range []string{"--insecure-listen", "the recorded design", "NO authentication"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: refusal does not mention %q, so it states a verdict without the "+
					"consequence or the way out: %v", addr, want, err)
			}
		}
	}
}

// THE POSITION, the half a pure classifier test does not deliver: New refuses BEFORE
// materializing anything. Same assertion and reason as
// TestTheAssemblyRefusesItsOwnUIDAsTheSecondHopUID. Root-free precisely because the check runs
// first — rootfs/runtime/btrfs are never reached.
func TestTheAssemblyRefusesAPublishedListenerBeforeMaterializingAnything(t *testing.T) {
	base := t.TempDir()
	_, err := New(Config{
		TemplatesDir: filepath.Join(base, "templates"),
		StateDir:     filepath.Join(base, "state"),
		StorePath:    ":memory:",
		ListenAddr:   "0.0.0.0:8080",
	})
	if err == nil {
		t.Fatal("the assembly accepted a listener on every interface — with no authentication and no " +
			"TLS in the engine (the documented invariant), that publishes a control plane able to create root-capable " +
			"sandboxes (the recorded design)")
	}
	if !errors.Is(err, ErrListenAddrNotLoopback) {
		t.Errorf("New failed for some other reason than the listen-address refusal: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "state")); statErr == nil {
		t.Error("the refusal happened AFTER the providers materialized state — it must be the last " +
			"point at which nothing exists yet and nothing has been served")
	}
}

// THE CONTROL ARM, asserting success rather than the absence of one error: the same
// config with a loopback listener assembles cleanly. Without this, the test above
// would keep passing if New started failing for any unrelated reason and never
// reached the address check at all.
func TestTheAssemblyAcceptsALoopbackListener(t *testing.T) {
	base := t.TempDir()
	eng, err := New(Config{
		TemplatesDir: filepath.Join(base, "templates"),
		StateDir:     filepath.Join(base, "state"),
		StorePath:    ":memory:",
		ListenAddr:   "127.0.0.1:8080",
	})
	if err != nil {
		t.Fatalf("a loopback listener was refused: %v — the check must reject a PUBLISHED listener, "+
			"not the flag", err)
	}
	if eng == nil {
		t.Fatal("New returned no engine and no error")
	}
	if _, statErr := os.Stat(filepath.Join(base, "state")); statErr != nil {
		t.Errorf("the control assembly did not materialize its state dir (%v) — then it never reached "+
			"the address check, and it is not a control on the refusal above", statErr)
	}
}

// --insecure-listen is what makes the refusal a DECISION rather than a wall. It buys
// exactly one thing: the classifier is not consulted. Both halves are asserted here,
// because a flag that silently did nothing and a flag that disabled something else
// would both pass a one-sided test.
func TestInsecureListenIsTheDeliberateOptOut(t *testing.T) {
	base := t.TempDir()
	eng, err := New(Config{
		TemplatesDir:   filepath.Join(base, "templates"),
		StateDir:       filepath.Join(base, "state"),
		StorePath:      ":memory:",
		ListenAddr:     "0.0.0.0:8080",
		InsecureListen: true,
	})
	if err != nil {
		t.Fatalf("--insecure-listen did not permit a published listener: %v — then the flag is a "+
			"dead end, and an operator who needs this shape patches the source instead, which takes "+
			"the decision out of the record entirely (the recorded design)", err)
	}
	if eng == nil {
		t.Fatal("New returned no engine and no error")
	}

	// And it does not disable the OTHER startup refusal. The two are independent
	// deployment checks; a flag about the listen address must not widen into a
	// general "skip the startup checks" switch.
	euid := os.Geteuid()
	if _, err := New(Config{
		TemplatesDir:   filepath.Join(base, "templates2"),
		StateDir:       filepath.Join(base, "state2"),
		StorePath:      ":memory:",
		ListenAddr:     "0.0.0.0:8080",
		InsecureListen: true,
		SecondHopUID:   &euid,
	}); err == nil {
		t.Error("--insecure-listen also suppressed the second-hop self-uid refusal — it must buy " +
			"exactly one thing, the listen-address classification")
	}
}

// An EMPTY ListenAddr is not validated, and that is what every in-process caller
// needs: a test serving through httptest.NewServer never listens on this address, so
// there is nothing to classify. Asserted rather than assumed, because the natural
// mistake in the other direction — treating "" as a routable listener — would refuse
// every existing Engine construction in the suite.
func TestAnAbsentListenAddrIsNotClassified(t *testing.T) {
	base := t.TempDir()
	eng, err := New(Config{
		TemplatesDir: filepath.Join(base, "templates"),
		StateDir:     filepath.Join(base, "state"),
		StorePath:    ":memory:",
	})
	if err != nil {
		t.Fatalf("New refused a Config with no ListenAddr: %v — an in-process Engine serves through "+
			"httptest and has no listen address to classify", err)
	}
	if eng == nil {
		t.Fatal("New returned no engine and no error")
	}
}

// A resolver is consulted ONLY for a name. A literal that reached LookupHost would be
// a live network call on the startup path — and on a host whose resolver is slow or
// absent, that turns a decidable local question into a boot-time hang.
func TestALiteralNeverReachesTheResolver(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "0.0.0.0:8080", "[::ffff:127.0.0.1]:8080"} {
		called := false
		resolve := func(string) ([]string, error) {
			called = true
			return nil, fmt.Errorf("resolver must not be consulted for an IP literal")
		}
		_ = ValidateListenAddr(addr, resolve)
		if called {
			t.Errorf("ValidateListenAddr(%q) consulted the resolver for an IP LITERAL — that puts a "+
				"network call on the startup path for a question answerable locally", addr)
		}
	}
}
