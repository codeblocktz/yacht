package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Proving a person may use an installation.
//
// GitHub sends somebody who installed the App back with installation_id in
// the query string, and anyone can type a number into a query string. With
// "Request user authorization (OAuth) during installation" on, it also sends
// a code, which only GitHub issues and only to the person who was there. The
// code buys a user token, and the user token lists the installations of this
// App that person can reach. An installation is connected only if it is in
// that list.
//
// The user token is used for that list and nothing else, then dropped. It is
// short-lived (GitHub expires user tokens by default), never stored, and
// never needed again: everything after connect is done as the installation.

// UserInstallations exchanges a code for a user token and lists the
// installations of this App the person behind it can reach, with their
// GitHub login.
func (a *App) UserInstallations(ctx context.Context, code string) (string, []Installation, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", nil, ErrNotAuthorized
	}
	token, err := a.exchangeCode(ctx, code)
	if err != nil {
		return "", nil, err
	}

	var user struct {
		Login string `json:"login"`
	}
	if _, err := a.do(ctx, http.MethodGet, a.cfg.APIURL+"/user", "Bearer "+token, nil, &user); err != nil {
		return "", nil, err
	}

	var all []Installation
	next := a.cfg.APIURL + "/user/installations?per_page=100"
	for page := 0; next != "" && page < maxPages; page++ {
		var out struct {
			Installations []Installation `json:"installations"`
		}
		link, err := a.do(ctx, http.MethodGet, next, "Bearer "+token, nil, &out)
		if err != nil {
			return "", nil, err
		}
		for _, in := range out.Installations {
			// The list is this App's already, for a token this App issued.
			// Checked anyway: an installation of some other App is not one
			// this install can mint tokens for.
			if in.AppID == 0 || in.AppID == a.cfg.AppID {
				all = append(all, in)
			}
		}
		next = a.sameHost(nextLink(link))
	}
	return user.Login, all, nil
}

// exchangeCode spends a code for a user token.
//
// GitHub answers a bad code with 200 and an error in the body, so the body
// is what decides.
func (a *App) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {a.cfg.ClientID},
		"client_secret": {a.cfg.ClientSecret},
		"code":          {code},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.cfg.WebURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.cfg.HTTP.Do(req)
	if err != nil {
		return "", errors.New("github: could not reach GitHub to finish signing in")
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil ||
		resp.StatusCode != http.StatusOK || out.Error != "" || out.AccessToken == "" {
		return "", ErrNotAuthorized
	}
	return out.AccessToken, nil
}

// State is what the install link carries through GitHub and back: which team
// asked, and who. Signed, so the callback can trust it named the team it
// says; checked there against the session that arrives with it, so a link
// made for one person cannot connect an installation for another.
//
// The same seal carries a choice: an installation GitHub has just listed as
// the person's, offered on a page for them to pick when there are several.
// The kind keeps the two apart, so neither can stand in for the other.
type State struct {
	Kind    string    `json:"k"`
	Team    string    `json:"t"`
	User    string    `json:"u"`
	Expires time.Time `json:"e"`

	// Installation and Login are a choice's: the installation, and the
	// GitHub account that proved access to it.
	Installation int64  `json:"i,omitempty"`
	Login        string `json:"l,omitempty"`
}

// The kinds of state.
const (
	StateLink   = "link"
	StateChoice = "choice"
)

// stateTTL is how long somebody has to get through GitHub's pages, or to
// pick from the installations GitHub listed.
const stateTTL = 30 * time.Minute

// ErrStateInvalid is a state that was not signed here, was altered, is the
// wrong kind, or is too old.
var ErrStateInvalid = errors.New("github: this GitHub link has expired or was not made here")

// SignState seals a state for the install link.
func (a *App) SignState(team, user string) string {
	return a.seal(State{Kind: StateLink, Team: team, User: user})
}

// SignChoice seals an installation the person was just shown to reach.
func (a *App) SignChoice(team, user string, installation int64, login string) string {
	return a.seal(State{
		Kind: StateChoice, Team: team, User: user, Installation: installation, Login: login,
	})
}

func (a *App) seal(s State) string {
	s.Expires = a.cfg.Now().Add(stateTTL).UTC()
	body, _ := json.Marshal(s)
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + base64.RawURLEncoding.EncodeToString(a.stateMAC(payload))
}

// OpenState checks a state of the given kind and returns what it says.
func (a *App) OpenState(raw, kind string) (State, error) {
	payload, sig, ok := strings.Cut(strings.TrimSpace(raw), ".")
	if !ok {
		return State{}, ErrStateInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, a.stateMAC(payload)) {
		return State{}, ErrStateInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return State{}, ErrStateInvalid
	}
	var s State
	if err := json.Unmarshal(body, &s); err != nil || s.Kind != kind || s.Team == "" ||
		!a.cfg.Now().Before(s.Expires) {
		return State{}, ErrStateInvalid
	}
	if kind == StateChoice && s.Installation <= 0 {
		return State{}, ErrStateInvalid
	}
	return s, nil
}

// stateMAC keys the state from the App's client secret, so every replica of
// this install signs and checks alike with nothing more to configure, and a
// state is worthless to anyone without the secret.
func (a *App) stateMAC(payload string) []byte {
	k := hmac.New(sha256.New, []byte(a.cfg.ClientSecret))
	k.Write([]byte("yacht github state v1"))
	mac := hmac.New(sha256.New, k.Sum(nil))
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}
