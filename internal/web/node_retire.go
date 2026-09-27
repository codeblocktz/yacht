package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/retire"
)

// Retiring a node is the gentle way out of service: the engine's apps move by
// rolling restart, one at a time, each replacement ready before its original
// stops. The drain beside it stays, labelled as the forceful option — it is
// still the answer when a machine is already gone and waiting is pointless.

// fillRetire adds the retirement plan to a node page.
//
// A plan that cannot be read leaves the drain as the only way out rather than
// failing the page: the page is where somebody goes when things are already
// going wrong, and it has to render then.
func (s *Server) fillRetire(r *http.Request, data *NodeDetailData) {
	if s.retirer == nil {
		return
	}
	plan, err := s.retirer.Plan(r.Context(), data.Node.Name)
	if err != nil {
		// A poll abandoned mid-read — the tab closed — is not a fault.
		if r.Context().Err() == nil {
			s.log.Error("read retirement plan", slog.String("node", data.Node.Name),
				slog.String("error", err.Error()))
		}
		return
	}
	data.Retire, data.CanRetire = plan, true
	if plan.Retiring() && plan.Empty() {
		data.Removable = true
	}
}

// nodeRetire starts retiring a node, or takes the next step of one under way.
func (s *Server) nodeRetire(w http.ResponseWriter, r *http.Request) {
	if s.retirer == nil {
		http.Error(w, "this cluster connection cannot move work gently", http.StatusNotImplemented)
		return
	}
	name := chi.URLParam(r, "name")

	plan, err := s.retirer.Begin(r.Context(), name)
	var noRoom *retire.NoRoomError
	if errors.As(err, &noRoom) {
		s.renderNodeError(w, r, errors.New(noRoomMessage(name, noRoom.Room)))
		return
	}
	if err != nil {
		// Whatever was done before the error stands, and the next pass in the
		// background picks up from it. Said, because the page will show the
		// retirement under way despite the error above it.
		s.log.Error("retire node", slog.String("node", name), slog.String("error", err.Error()))
		s.renderNodeError(w, r, err)
		return
	}
	s.flashOK(w, r, retireStartedMessage(name, plan))
	http.Redirect(w, r, "/cluster/nodes/"+name, http.StatusSeeOther)
}

// nodeRetireStop abandons a retirement and reopens the node.
func (s *Server) nodeRetireStop(w http.ResponseWriter, r *http.Request) {
	if s.retirer == nil {
		http.Error(w, "this cluster connection cannot move work gently", http.StatusNotImplemented)
		return
	}
	name := chi.URLParam(r, "name")
	if err := s.retirer.Stop(r.Context(), name); err != nil {
		s.renderNodeError(w, r, err)
		return
	}
	s.flashOK(w, r, "Stopped retiring "+name+". It takes new work again; apps that already moved stay where they went.")
	http.Redirect(w, r, "/cluster/nodes/"+name, http.StatusSeeOther)
}

// noRoomMessage says what is missing, in the units somebody would buy a
// machine in, and what to do about it.
func noRoomMessage(node string, room orchestrator.RetireRoom) string {
	msg := "The other nodes cannot hold what " + node + " is running"
	short := ""
	if c := room.ShortCPUMillis(); c > 0 {
		short = formatMillicores(c) + " CPU"
	}
	if m := room.ShortMemBytes(); m > 0 {
		if short != "" {
			short += " and "
		}
		short += formatBytes(m) + " of memory"
	}
	switch {
	case short != "":
		msg += ": they are short of " + short + "."
	case room.Unplaced != "":
		msg += ": " + room.Unplaced + " asks for more than any one of them has free."
	default:
		msg += "."
	}
	return msg + " Add a node first, then retire this one — nothing has been changed."
}

func retireStartedMessage(node string, plan orchestrator.RetirePlan) string {
	if len(plan.Apps) == 0 {
		return "Retiring " + node + ". No apps need to move."
	}
	return "Retiring " + node + ". " + plural(len(plan.Apps), "app") +
		" move one at a time, each replacement ready before the original stops."
}
