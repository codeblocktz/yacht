package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Sleeping apps: the owner's side — the setting, the buttons, the history —
// and, in waker.go, the handler a sleeping app's requests are routed to.

// Sleeper is what the dashboard needs to put apps to sleep and wake them.
//
// Its own interface rather than more of Apps, like Quotas and Capacity: an
// application wrapping the engine may supply one without the other, and nil
// leaves every sleep control off the pages.
type Sleeper interface {
	// CanSleep reports whether the install has a waker at all.
	CanSleep() bool

	SleepStatus(ctx context.Context, ownerID, name string) (app.SleepStatus, error)
	SetSleepSetting(ctx context.Context, ownerID, name string, in app.SleepSetting) (app.App, error)
	SleepNow(ctx context.Context, ownerID, name string) error
	Wake(ctx context.Context, ownerID, name string) error

	TeamSleepDefault(ctx context.Context, ownerID string) (app.SleepPolicy, error)
	SetTeamSleepDefault(ctx context.Context, ownerID string, p app.SleepPolicy) error

	// The waker's: which app a hostname is, wake it, and where it answers.
	WakeTarget(ctx context.Context, host string) (app.App, error)
	WakeForRequest(ctx context.Context, a app.App) error
	Backend(ctx context.Context, a app.App) (string, error)
}

var _ Sleeper = (*app.Service)(nil)

// wakeButtonHold is how long the Wake now button waits before answering. A
// wake that finishes inside it is reported done; one that does not carries on
// without the request, and the page says it is on its way.
const wakeButtonHold = 3 * time.Second

// attachSleep reads the app's sleeping for the panel.
func (s *Server) attachSleep(ctx context.Context, d *AppDetailData) {
	if s.sleep == nil {
		return
	}
	st, err := s.sleep.SleepStatus(ctx, d.App.OwnerID, d.App.Name)
	if err != nil {
		s.log.Error("read sleep status", slog.String("app", d.App.Name), slog.String("error", err.Error()))
		return
	}
	d.Sleep = &st
}

// appSleepNow puts an app to sleep by hand.
func (s *Server) appSleepNow(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")
	if err := s.sleep.SleepNow(r.Context(), owner.ID, name); err != nil {
		s.log.Warn("sleep now", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, sleepRefusal(err))
		http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
		return
	}
	s.flashOK(w, r, name+" is asleep. The next request to it wakes it.")
	http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
}

// appWakeNow wakes an app by hand, waiting a moment for it.
func (s *Server) appWakeNow(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")
	ctx, cancel := context.WithTimeout(r.Context(), wakeButtonHold)
	defer cancel()
	err := s.sleep.Wake(ctx, owner.ID, name)
	switch {
	case err == nil:
		s.flashOK(w, r, name+" is awake.")
	case errors.Is(err, context.DeadlineExceeded):
		s.flashOK(w, r, "Waking "+name+" — it will be serving in a moment.")
	default:
		s.log.Warn("wake now", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, sleepRefusal(err))
	}
	http.Redirect(w, r, "/apps/"+name, http.StatusSeeOther)
}

// appSleepSetting saves an app's own setting.
func (s *Server) appSleepSetting(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")
	back := "/apps/" + name + "/settings#sleep"

	in := app.SleepSetting{Mode: app.SleepMode(r.FormValue("mode"))}
	after, err := sleepMinutes(r.FormValue("after"))
	if err == nil {
		in.After = after
		_, err = s.sleep.SetSleepSetting(r.Context(), owner.ID, name, in)
	}
	if err != nil {
		s.log.Warn("set sleep setting", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, sleepRefusal(err))
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	s.flashOK(w, r, "Sleep setting saved.")
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// teamSleepDefault saves the team's default, for every app left to inherit it.
func (s *Server) teamSleepDefault(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	after, err := sleepMinutes(r.FormValue("after"))
	if err == nil {
		p := app.SleepPolicy{Enabled: formChecked(r, "enabled"), After: after}
		if p.After == 0 {
			p.After = app.DefaultSleepAfter
		}
		err = s.sleep.SetTeamSleepDefault(r.Context(), owner.ID, p)
	}
	if err != nil {
		s.log.Warn("set team sleep default", slog.String("error", err.Error()))
		s.flashErr(w, r, sleepRefusal(err))
		http.Redirect(w, r, "/team#sleep", http.StatusSeeOther)
		return
	}
	s.flashOK(w, r, "Team sleep default saved. Apps that follow it pick it up on the next check.")
	http.Redirect(w, r, "/team#sleep", http.StatusSeeOther)
}

// sleepMinutes reads an idle time typed in minutes; empty is zero, which
// means "the team's" for an app and "the default" for a team.
func sleepMinutes(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("the idle time is a whole number of minutes")
	}
	return time.Duration(n) * time.Minute, nil
}

// sleepRefusal is an error from the sleep service as a person reads it: the
// engine's own sentence without its package prefix.
func sleepRefusal(err error) string {
	msg := err.Error()
	if errors.Is(err, app.ErrOperationInFlight) {
		return "A deploy is in progress. An app is never put to sleep mid-deploy — try again once it has finished."
	}
	msg = strings.TrimPrefix(msg, "app: ")
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	return msg
}

// sleepModeLabel names a setting in the app's form.
func sleepModeLabel(m app.SleepMode, team app.SleepPolicy) string {
	switch m {
	case app.SleepOn:
		return "Sleep when idle"
	case app.SleepOff:
		return "Never sleep"
	}
	return sleepFollowsTeam(team)
}

// sleepFollowsTeam describes an inheriting app's effective setting.
func sleepFollowsTeam(team app.SleepPolicy) string {
	if !team.Enabled {
		return "Team default — off"
	}
	return "Team default — after " + minutesLabel(team.After)
}

// minutesLabel says an idle time in minutes, or hours where it is whole ones.
func minutesLabel(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return plural(int(d/time.Hour), "hour")
	}
	return strconv.Itoa(int(d/time.Minute)) + " min"
}

// minutesValue is an idle time for a form field, empty for zero.
func minutesValue(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return strconv.Itoa(int(d / time.Minute))
}

// lastRequestLabel says when an app was last asked for, as a sentence tail.
func lastRequestLabel(sl app.Sleep) string {
	if sl.LastRequestAt == nil {
		return "no request seen yet"
	}
	return "last request " + relativeTime(*sl.LastRequestAt)
}

// sleepingTitle is a sleeping app's status, spelled out on hover.
func sleepingTitle(sl app.Sleep) string {
	out := "Asleep"
	if sl.Since != nil {
		out += " since " + relativeTime(*sl.Since)
	}
	return out + " — " + lastRequestLabel(sl) + ". The next request wakes it."
}

// sleptFor is how long a finished sleep lasted, coarsely.
func sleptFor(i app.SleepInterval) string {
	if i.To == nil {
		return ""
	}
	d := i.To.Sub(i.From)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	}
	return plural(int(d.Hours()/24), "day")
}

// recentWakeFailure is the reason the last wake failed, when it is recent
// enough to still be the answer.
func recentWakeFailure(sl app.Sleep) string {
	err := app.RecentWakeFailure(app.App{Sleep: sl}, time.Now())
	switch {
	case errors.Is(err, app.ErrWakeNoRoom):
		return "The last wake was refused: the install had no room for it. The next request after a minute tries again."
	case errors.Is(err, app.ErrWakeTimeout):
		return "The last wake timed out before the app was ready, so it went back to sleep."
	}
	return ""
}
