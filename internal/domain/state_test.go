package domain

import (
	"errors"
	"testing"
	"time"
)

// The states a lookup can land a domain in, from each state it can start in.
//
// Table-driven because the interesting part is the whole matrix rather than any
// one cell: the bug this guards against is a transition nobody thought about,
// not a transition somebody got backwards.
func TestClassify(t *testing.T) {
	resolvesHere := Observation{Resolves: true, PointsHere: true}
	resolvesElsewhere := Observation{Resolves: true, CNAME: "somewhere.else.test."}
	resolvesNowhere := Observation{}
	lookupBroke := Observation{Err: errors.New("resolver unreachable")}

	for _, tc := range []struct {
		name     string
		obs      Observation
		previous State
		want     State
	}{
		{"a fresh claim that already resolves", resolvesHere, StatePending, StateVerified},
		{"a fresh claim with no record yet", resolvesNowhere, StatePending, StateAwaitingDNS},
		{"a fresh claim pointing elsewhere", resolvesElsewhere, StatePending, StateMisdirected},

		{"still waiting", resolvesNowhere, StateAwaitingDNS, StateAwaitingDNS},
		{"the record arrives", resolvesHere, StateAwaitingDNS, StateVerified},
		{"the record arrives wrong", resolvesElsewhere, StateAwaitingDNS, StateMisdirected},
		{"the wrong record is corrected", resolvesHere, StateMisdirected, StateVerified},

		// Routed is the state with memory. Falling out of it is drift, which is
		// a different sentence from never having worked.
		{"a live domain is deleted at the provider", resolvesNowhere, StateRouted, StateDrifted},
		{"a live domain is repointed elsewhere", resolvesElsewhere, StateRouted, StateDrifted},
		{"a live domain stays live", resolvesHere, StateRouted, StateRouted},
		{"drift is repaired", resolvesHere, StateDrifted, StateVerified},
		{"drift persists", resolvesNowhere, StateDrifted, StateAwaitingDNS},

		// A resolver that cannot answer says nothing about the domain. An
		// outage must not mark every custom domain on the install as broken.
		{"a broken resolver leaves a live domain alone", lookupBroke, StateRouted, StateRouted},
		{"a broken resolver leaves a waiting domain alone", lookupBroke, StateAwaitingDNS, StateAwaitingDNS},
		{"a broken resolver on a brand new claim", lookupBroke, "", StatePending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.obs, tc.previous); got != tc.want {
				t.Errorf("Classify(%+v, %q) = %q, want %q", tc.obs, tc.previous, got, tc.want)
			}
		})
	}
}

// Routable is what the Ingress is gated on, and the database generates the
// verified column from exactly this set. If the two ever disagree, a domain is
// either serving traffic it should not or not serving traffic it should.
func TestOnlyProvenStatesAreRoutable(t *testing.T) {
	routable := map[State]bool{StateVerified: true, StateRouted: true}
	for _, s := range []State{
		StatePending, StateAwaitingDNS, StateMisdirected,
		StateVerified, StateRouted, StateDrifted,
	} {
		if got := s.Routable(); got != routable[s] {
			t.Errorf("%q.Routable() = %v, want %v", s, got, routable[s])
		}
	}
}

// Only routed is finished. Drifted is a completed check whose result somebody
// has to act on, and treating it as settled is how a broken domain stops being
// re-checked.
func TestOnlyRoutedIsSettled(t *testing.T) {
	if !StateRouted.Settled() {
		t.Error("routed should be settled")
	}
	for _, s := range []State{
		StatePending, StateAwaitingDNS, StateMisdirected, StateVerified, StateDrifted,
	} {
		if s.Settled() {
			t.Errorf("%q should not be settled", s)
		}
	}
}

// The backoff exists so that somebody watching a fresh claim is served quickly
// and a domain that has been live for a month is not asked about every ten
// seconds forever.
func TestNextCheckBacksOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    State
		attempts int
		want     time.Duration
	}{
		{"a claim just made", StateAwaitingDNS, 1, checkEager},
		{"still early", StateAwaitingDNS, 3, checkEager},
		{"a minute in", StateAwaitingDNS, 5, checkSoon},
		{"several minutes in", StateAwaitingDNS, 10, checkSteady},
		{"a long wait", StateAwaitingDNS, 20, checkSlow},
		{"given up watching", StateAwaitingDNS, 40, checkIdle},

		// State wins over attempts: a live domain is re-proven on its own
		// schedule however many times it has been checked.
		{"live", StateRouted, 1, settledRecheck},
		{"live for a long time", StateRouted, 500, settledRecheck},
		{"broken", StateDrifted, 1, driftRecheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextCheck(tc.state, tc.attempts); got != tc.want {
				t.Errorf("NextCheck(%q, %d) = %v, want %v", tc.state, tc.attempts, got, tc.want)
			}
		})
	}
}

// The interval must never shrink as attempts pile up. A backoff that went
// backwards would turn an abandoned claim into a permanent load on DNS.
func TestBackoffNeverShrinks(t *testing.T) {
	var previous time.Duration
	for attempts := 1; attempts <= 100; attempts++ {
		got := NextCheck(StateAwaitingDNS, attempts)
		if got < previous {
			t.Fatalf("attempt %d waits %v, less than the %v before it", attempts, got, previous)
		}
		previous = got
	}
}
