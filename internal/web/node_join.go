package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/cluster"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// Joiner is the dashboard's view of how a machine joins this cluster.
//
// An interface rather than the concrete type, for the reason Apps is one: the
// page has to be testable without a database, and this package has no business
// knowing how a join token is stored.
type Joiner interface {
	Settings(ctx context.Context) (cluster.Settings, error)
	SetJoin(ctx context.Context, serverURL, token string, by uuid.UUID) error
	Command(ctx context.Context, pool string) (string, error)
}

// justJoined is how recently a node must have been created to be called new.
//
// Read from the node's own creation time rather than from a snapshot taken when
// the page was opened. A "what changed since you loaded" approach gets the
// common case wrong: open the page after the node has already joined and it
// reports nothing, which is the same thing it reports when the join failed.
const justJoined = 15 * time.Minute

// AddNodeData is the page that hands over a join command.
type AddNodeData struct {
	// Settings describes what is stored. It never carries the token — see
	// cluster.Settings.
	Settings   cluster.Settings
	Configured bool

	// Command is the line to run. Empty until settings exist, because a
	// command with holes in it is worse than a prompt to fill them in.
	Command string
	Pool    string

	// Nodes and ClusterOK are the confirmation half.
	Nodes     []orchestrator.NodeInfo
	ClusterOK bool

	Error string
}

// IsNew reports whether a node joined recently enough to be worth pointing at.
func (d AddNodeData) IsNew(n orchestrator.NodeInfo) bool {
	return !n.CreatedAt.IsZero() && time.Since(n.CreatedAt) < justJoined
}

// nodeAdd renders the join command and watches for the node arriving.
func (s *Server) nodeAdd(w http.ResponseWriter, r *http.Request) {
	data := s.addNodeData(r)
	s.renderWithCrumb(w, r, AddNode(data), "Add node")
}

// nodeAddFragment is the polled half of the page.
//
// Its own endpoint rather than a whole-page refresh: the command is on screen
// and probably half-selected, and re-rendering it under the cursor every few
// seconds would make it impossible to copy.
func (s *Server) nodeAddFragment(w http.ResponseWriter, r *http.Request) {
	data := s.addNodeData(r)
	if err := AddNodeNodes(data).Render(r.Context(), w); err != nil {
		s.log.Error("render node poll", slog.String("error", err.Error()))
	}
}

func (s *Server) addNodeData(r *http.Request) AddNodeData {
	ctx := r.Context()
	data := AddNodeData{Pool: r.URL.Query().Get("pool")}

	if err := cluster.ValidatePool(data.Pool); err != nil {
		data.Error = err.Error()
		data.Pool = ""
	}

	settings, err := s.joiner.Settings(ctx)
	switch {
	case err == nil:
		data.Settings, data.Configured = settings, true
	case errors.Is(err, cluster.ErrNotConfigured):
		// Not an error on this page: it is the state the page exists to get
		// out of, and it has its own prompt.
	default:
		s.log.Error("read join settings", slog.String("error", err.Error()))
		data.Error = "Could not read the join settings."
	}

	if data.Configured && data.Error == "" {
		cmd, err := s.joiner.Command(ctx, data.Pool)
		if err != nil {
			// Named rather than swallowed. The usual cause is a rotated key,
			// and "no command" with no reason sends somebody to read logs.
			s.log.Error("build join command", slog.String("error", err.Error()))
			data.Error = err.Error()
		} else {
			data.Command = cmd
		}
	}

	// The command does not need the cluster, so an unreachable cluster stops
	// the confirmation and nothing else.
	if err := s.orch.Ping(ctx); err == nil {
		data.ClusterOK = true
		if nodes, err := s.orch.Nodes(ctx); err == nil {
			data.Nodes = nodes
		}
	}
	return data
}

// nodeJoinSet stores the server address and token.
func (s *Server) nodeJoinSet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var by uuid.UUID
	if s.accounts != nil {
		if sess, err := s.accounts.ResolveSession(ctx, sessionToken(r)); err == nil {
			by = sess.UserID
		}
	}

	err := s.joiner.SetJoin(ctx, r.FormValue("server_url"), r.FormValue("token"), by)
	if err != nil {
		// Re-rendered rather than redirected, so the refusal arrives with the
		// form that caused it instead of as a detail on another page.
		data := s.addNodeData(r)
		data.Error = err.Error()
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.renderWithCrumb(w, r, AddNode(data), "Add node")
		return
	}
	http.Redirect(w, r, "/cluster/nodes/add", http.StatusSeeOther)
}
