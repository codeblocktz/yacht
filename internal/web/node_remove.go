package web

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// nodeManager returns the orchestrator's node-management ability, if it has one.
//
// Asserted rather than required, matching the interface: an implementation
// backed by a restricted credential cannot evict across every namespace, and
// the surface stays off rather than failing when somebody presses it.
func (s *Server) nodeManager() (orchestrator.NodeManager, bool) {
	nm, ok := s.orch.(orchestrator.NodeManager)
	return nm, ok
}

// NodeDetailData is one machine and what is still running on it.
type NodeDetailData struct {
	Node  orchestrator.NodeInfo
	Found bool

	// Pods are what remains on the node. Drained is true when nothing is left
	// that a drain would move, which is the condition for offering removal.
	Pods    []orchestrator.PodInfo
	Drained bool

	// CanManage is false when the orchestrator cannot take a node out of
	// service, so the page explains rather than offering buttons that fail.
	CanManage bool

	Error  string
	Notice string
}

// nodeDetail shows one node and the actions available on it.
func (s *Server) nodeDetail(w http.ResponseWriter, r *http.Request) {
	data := s.nodeDetailData(r, "")
	if !data.Found {
		http.NotFound(w, r)
		return
	}
	s.renderWithCrumb(w, r, NodeDetail(data), data.Node.Name)
}

// nodeDetailFragment is the polled half: draining is asynchronous, so the page
// watches the node empty rather than holding a request open while it does.
func (s *Server) nodeDetailFragment(w http.ResponseWriter, r *http.Request) {
	data := s.nodeDetailData(r, "")
	if err := NodeDetailBody(data).Render(r.Context(), w); err != nil {
		s.log.Error("render node poll", slog.String("error", err.Error()))
	}
}

func (s *Server) nodeDetailData(r *http.Request, notice string) NodeDetailData {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	data := NodeDetailData{Notice: notice}

	_, data.CanManage = s.nodeManager()

	nodes, err := s.orch.Nodes(ctx)
	if err != nil {
		s.log.Error("list nodes", slog.String("error", err.Error()))
		data.Error = "The cluster is not answering."
		return data
	}
	for _, n := range nodes {
		if n.Name == name {
			data.Node, data.Found = n, true
			break
		}
	}
	if !data.Found {
		return data
	}

	// Every namespace, because a node runs whatever was scheduled onto it and
	// an operator emptying one needs to see all of it. This page is owner-only
	// for exactly that reason.
	pods, err := s.orch.Pods(ctx, orchestrator.PodListOptions{Node: name})
	if err != nil {
		s.log.Error("list pods on node", slog.String("error", err.Error()))
		data.Error = "Could not read what is running on this node."
		return data
	}
	data.Pods = pods

	// Drained means nothing is left that a drain would move. The orchestrator
	// decides which those are, because it is what does the moving — counting
	// here would be a second copy of that rule.
	data.Drained = true
	for _, p := range pods {
		if p.DrainMoves {
			data.Drained = false
			break
		}
	}
	return data
}

// nodeCordon stops or resumes scheduling onto a node.
func (s *Server) nodeCordon(w http.ResponseWriter, r *http.Request) {
	nm, ok := s.nodeManager()
	if !ok {
		http.Error(w, "this cluster connection cannot manage nodes", http.StatusNotImplemented)
		return
	}
	name := chi.URLParam(r, "name")
	on := r.FormValue("unschedulable") == "true"

	if err := nm.Cordon(r.Context(), name, on); err != nil {
		s.renderNodeError(w, r, err)
		return
	}
	s.log.Info("node cordon changed", slog.String("node", name), slog.Bool("unschedulable", on))
	http.Redirect(w, r, "/cluster/nodes/"+name, http.StatusSeeOther)
}

// nodeDrain cordons and then asks the pods to leave.
//
// Cordoning first is not a convenience: draining a node that still accepts work
// races the scheduler, and a pod can land on the machine being emptied.
func (s *Server) nodeDrain(w http.ResponseWriter, r *http.Request) {
	nm, ok := s.nodeManager()
	if !ok {
		http.Error(w, "this cluster connection cannot manage nodes", http.StatusNotImplemented)
		return
	}
	ctx := r.Context()
	name := chi.URLParam(r, "name")

	if err := nm.Cordon(ctx, name, true); err != nil {
		s.renderNodeError(w, r, err)
		return
	}
	requested, err := nm.Drain(ctx, name)
	if err != nil {
		// The cordon stands. Reporting the refusal with the node still closed
		// to new work is the honest state: some pods were asked to leave, and
		// the operator has to decide about the rest.
		s.log.Error("drain node", slog.String("node", name), slog.String("error", err.Error()))
		s.renderNodeError(w, r, err)
		return
	}
	s.log.Info("node drained", slog.String("node", name), slog.Int("evictions", requested))
	http.Redirect(w, r, "/cluster/nodes/"+name, http.StatusSeeOther)
}

// nodeRemove deletes the node object.
func (s *Server) nodeRemove(w http.ResponseWriter, r *http.Request) {
	nm, ok := s.nodeManager()
	if !ok {
		http.Error(w, "this cluster connection cannot manage nodes", http.StatusNotImplemented)
		return
	}
	ctx := r.Context()
	name := chi.URLParam(r, "name")

	// Refused unless the node is actually empty. The button is only offered on
	// a drained node, but a form can be submitted from a page that has gone
	// stale, and this is the check that is not a rendering decision.
	data := s.nodeDetailData(r, "")
	if !data.Found {
		http.NotFound(w, r)
		return
	}
	if !data.Drained {
		s.renderNodeError(w, r, errors.New(
			"this node is still running workloads — drain it before removing it"))
		return
	}

	if err := nm.DeleteNode(ctx, name); err != nil {
		s.renderNodeError(w, r, err)
		return
	}
	s.log.Info("node removed", slog.String("node", name))
	http.Redirect(w, r, "/cluster/nodes", http.StatusSeeOther)
}

func (s *Server) renderNodeError(w http.ResponseWriter, r *http.Request, cause error) {
	data := s.nodeDetailData(r, "")
	data.Error = cause.Error()
	w.WriteHeader(http.StatusUnprocessableEntity)
	s.renderWithCrumb(w, r, NodeDetail(data), data.Node.Name)
}
