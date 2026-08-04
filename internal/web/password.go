package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/codeblocktz/yacht/internal/account"
)

// passwordFloor is the minimum time a password attempt takes.
//
// Above the cost of an Argon2id verify on purpose, so that the pad is always a
// wait rather than a race the fast path can lose. The service already does a
// dummy verify for an address it does not know, which equalises the expensive
// half by construction; this covers the residue — a row read against a row not
// read — and leaves headroom for a slow box or a later increase in the hashing
// cost.
//
// signInFloor is not reused because Argon2id is already most of a quarter
// second, and a floor that the work underneath it can exceed is not a floor.
const passwordFloor = 500 * time.Millisecond

// Attempts allowed in a window, per address and per client.
//
// The same numbers as the link limits, for a different reason: there the brake
// is on enumeration, here it is on guessing. Five attempts per address per
// fifteen minutes is 480 a day against a twelve-character minimum, which is not
// a rate at which anything is found, and is far more than somebody who knows
// their own password will ever need.
//
// They are counted under their own key prefixes rather than sharing the magic
// link's. Sharing would let an attacker spend a victim's link budget by guessing
// at their password — locking them out of the method they actually use, through
// the one they do not. Kept apart, the per-address password limit can never lock
// anybody out of their account at all, because the link is still there. That is
// what makes a limit this tight safe to have.
const (
	passwordAttemptsPerAddress = 5
	passwordAttemptsPerIP      = 20
)

// errCredentials is the only thing a failed password attempt ever says.
//
// An unknown address, an address with no password set, and the wrong password
// are one message on purpose: any wording that separated them would answer "does
// this person have an account, and have they chosen a password?", which is the
// pair of facts the whole page is built to withhold. The service returns one
// error for all three, so this handler could not tell them apart if it tried.
const errCredentials = signInError(
	"That email address and password do not match.")

// signInWithPassword checks a password and opens a session on it.
//
// Its own route rather than a second branch of POST /sign-in, and that is a
// security decision rather than a tidiness one. csrfProtect exempts origin-less
// posts on one path by name — /sign-in — so that a CLI with no browser to send
// an Origin can still ask for a link. Today that exemption is worth nothing to
// an attacker: the link goes to a mailbox they must already control. Accepting a
// password on the same path would turn it into login CSRF, where an origin-less
// cross-site post signs a victim's browser into the attacker's account and
// everything they do next — every variable, every connected repository — lands
// somewhere it can be read.
//
// On a path the exemption does not name, the existing middleware refuses that
// with no new logic. The alternative, narrowing the exemption to "no password
// field present", would make a security middleware parse request bodies.
func (s *Server) signInWithPassword(w http.ResponseWriter, r *http.Request) {
	email, err := parseEmail(r.FormValue("email"))
	if err != nil {
		// Without the floor, as the link form does it: a malformed address is a
		// mistake the visitor can already see, not a fact about who has an
		// account, and padding it only punishes a typo.
		s.refusePassword(w, r, http.StatusUnprocessableEntity,
			r.FormValue("email"), signInError(err.Error()))
		return
	}
	password := r.FormValue("password")

	// Both counters asked, never short-circuited, so an address already over its
	// limit still counts against the client that keeps trying it. Lowercased for
	// the same reason the link form lowercases: otherwise alternating the
	// capitalisation of one address buys an unlimited budget.
	addressOK := s.signInLimit.allow(
		"password:"+strings.ToLower(email), passwordAttemptsPerAddress)
	clientOK := s.signInLimit.allow(
		"password-client:"+clientIP(r), passwordAttemptsPerIP)
	if !addressOK || !clientOK {
		// Outside the floor, and now for a second reason as well as the original
		// one. Refusal depends only on the attempt count, so answering quickly
		// leaks nothing — and a padded refusal would hold a goroutine for half a
		// second per attack request, which is amplification handed to exactly the
		// traffic being shed.
		s.refusePassword(w, r, http.StatusTooManyRequests, email,
			"Too many sign-in attempts. Try again in a few minutes.")
		return
	}

	// An empty password is a failed credential and not a shortcut to the other
	// form. A route that sometimes mailed a link would be a route whose name
	// lies, and it would put the mail path under a different set of counters.
	var user account.User
	var authErr error
	withFloor(passwordFloor, func() {
		user, authErr = s.accounts.AuthenticatePassword(r.Context(), email, password)
	})
	if authErr != nil {
		if !errors.Is(authErr, account.ErrCredentialInvalid) {
			s.log.Error("authenticate password", slog.String("error", authErr.Error()))
		}
		s.refusePassword(w, r, http.StatusUnauthorized, email, errCredentials)
		return
	}

	if !s.startSession(w, r, user, account.MethodPassword) {
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// refusePassword renders the sign-in page back with the address in the form it
// was typed in.
//
// One place, so that every refusal on this route agrees on the status it pairs
// with a message and on which form the error is drawn beside.
func (s *Server) refusePassword(
	w http.ResponseWriter, r *http.Request, status int, email string, msg signInError,
) {
	s.renderSignedOut(w, r, status, "Sign in", SignIn(SignInData{
		Email:  email,
		Error:  string(msg),
		Method: SignInByPassword,
	}))
}
