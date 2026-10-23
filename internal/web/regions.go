package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/codeblocktz/yacht/internal/app"
	"github.com/codeblocktz/yacht/internal/identity"
)

// Regions is the dashboard's view of where apps live.
//
// Its own interface, like Capacity, because a wrapper may supply it without
// the rest, and because two of its methods read across every team.
type Regions interface {
	// Regions is every region the machines carry or the operator has
	// configured, with what is in each. Every team's apps are counted.
	Regions(ctx context.Context) ([]app.RegionInfo, error)

	// Region is one of them.
	Region(ctx context.Context, name string) (app.RegionInfo, error)

	// SetRegion configures one: name, edge, StorageClass, open.
	SetRegion(ctx context.Context, r app.Region) error

	// RegionChoice is what a deploy form offers.
	RegionChoice(ctx context.Context) (app.RegionChoice, error)

	// AppRegion is the region an app lives in.
	AppRegion(ctx context.Context, ownerID, name string) (string, error)

	// MoveApp redeploys an app in another region, refusing one with storage.
	MoveApp(ctx context.Context, ownerID, name, region string) error
}

var _ Regions = (*app.Service)(nil)

// RegionsData is the Admin area's regions page.
type RegionsData struct {
	OK    bool
	Error string

	Regions []app.RegionInfo

	// Capacity is each region's room by name, where the install has a
	// capacity policy to read it from.
	Capacity map[string]app.RegionCapacity

	CanAddNode bool
}

// RegionFormData is one region's settings, as a form.
type RegionFormData struct {
	Info app.RegionInfo

	// The fields as typed, so a refusal keeps them.
	DisplayName  string
	EdgeHost     string
	EdgeAddrs    string
	StorageClass string
	Open         bool

	Error string
}

// regionForm fills the form from what is stored. A region nobody has
// configured is offered open, since configuring one is almost always to put
// apps in it.
func regionForm(info app.RegionInfo) RegionFormData {
	f := RegionFormData{
		Info: info, DisplayName: info.DisplayName, EdgeHost: info.Edge.Host,
		EdgeAddrs: strings.Join(info.Edge.Addresses, "\n"), StorageClass: info.StorageClass,
		Open: info.Open || !info.Configured,
	}
	return f
}

func (s *Server) adminRegions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := RegionsData{OK: true, CanAddNode: s.joiner != nil}
	regions, err := s.regions.Regions(ctx)
	if err != nil {
		s.log.Error("list regions", slog.String("error", err.Error()))
		d.OK, d.Error = false, "Could not read the install's regions."
	}
	d.Regions = regions
	if s.capacity != nil {
		if snap, err := s.capacity.Capacity(ctx); err == nil {
			d.Capacity = make(map[string]app.RegionCapacity, len(snap.Regions))
			for _, rc := range snap.Regions {
				d.Capacity[rc.Region] = rc
			}
		} else {
			s.log.Error("read capacity for regions", slog.String("error", err.Error()))
		}
	}
	s.render(w, r, AdminRegions(d))
}

func (s *Server) adminRegion(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	info, err := s.regions.Region(r.Context(), name)
	if err != nil {
		if errors.Is(err, app.ErrNoSuchRegion) {
			// A region a machine might carry tomorrow can be described
			// today, so an installer can configure one before its machines
			// arrive. The name still has to be one.
			if verr := (app.Region{Name: name}).Validate(); verr != nil {
				http.NotFound(w, r)
				return
			}
			info = app.RegionInfo{Region: app.Region{Name: name}}
		} else {
			s.log.Error("read region", slog.String("error", err.Error()))
			http.Error(w, "could not read the region", http.StatusInternalServerError)
			return
		}
	}
	s.renderRegion(w, r, http.StatusOK, regionForm(info))
}

func (s *Server) renderRegion(w http.ResponseWriter, r *http.Request, status int, f RegionFormData) {
	slots := s.slots.Slots(r.Context(), r)
	slots.Breadcrumb = append(slots.Breadcrumb, Crumb{Label: f.Info.Label()})
	s.renderWithSlotsStatus(w, r, slots, status, AdminRegion(f))
}

func (s *Server) adminRegionSet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	f := RegionFormData{
		DisplayName:  strings.TrimSpace(r.FormValue("display_name")),
		EdgeHost:     strings.TrimSpace(r.FormValue("edge_host")),
		EdgeAddrs:    r.FormValue("edge_addresses"),
		StorageClass: strings.TrimSpace(r.FormValue("storage_class")),
		Open:         formChecked(r, "open"),
	}
	region := app.Region{
		Name: name, DisplayName: f.DisplayName,
		Edge:         app.Edge{Host: f.EdgeHost, Addresses: splitAddresses(f.EdgeAddrs)},
		StorageClass: f.StorageClass, Open: f.Open,
	}
	err := s.regions.SetRegion(r.Context(), region)
	if err != nil {
		s.log.Warn("set region", slog.String("region", name), slog.String("error", err.Error()))
		info, rerr := s.regions.Region(r.Context(), name)
		if rerr != nil {
			info = app.RegionInfo{Region: app.Region{Name: name}}
		}
		f.Info, f.Error = info, strings.TrimPrefix(err.Error(), "app: ")
		s.renderRegion(w, r, http.StatusUnprocessableEntity, f)
		return
	}
	msg := "Region " + name + " saved."
	if !f.Open {
		msg += " It takes no new apps; the ones already there stay."
	}
	s.flashOK(w, r, msg)
	http.Redirect(w, r, "/admin/regions", http.StatusSeeOther)
}

// splitAddresses reads one address per line, or several separated by commas
// or spaces — however they were pasted.
func splitAddresses(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
}

// regionChoice reads what a deploy form offers, or nothing — on an install
// with no regions, and when it cannot be read, in which case the create
// itself decides and says why if it refuses.
func (s *Server) regionChoice(ctx context.Context) app.RegionChoice {
	if s.regions == nil {
		return app.RegionChoice{}
	}
	c, err := s.regions.RegionChoice(ctx)
	if err != nil {
		s.log.Error("read region choice", slog.String("error", err.Error()))
		return app.RegionChoice{}
	}
	return c
}

// AppRegionData is the region an app lives in, for its page.
type AppRegionData struct {
	// Name is the region; Label what to call it.
	Name  string
	Label string

	// Recorded is false for an app made before regions existed whose region
	// is not recorded yet: it is taken to be in Name, and its pods learn so
	// at its next deploy.
	Recorded bool

	// Options are the other regions it could move to.
	Options []app.RegionInfo

	// HasStorage is why it cannot.
	HasStorage bool
}

// attachRegion reads where the app lives, on an install with regions.
func (s *Server) attachRegion(ctx context.Context, d *AppDetailData) {
	if s.regions == nil {
		return
	}
	choice := s.regionChoice(ctx)
	if !choice.InUse {
		return
	}
	name, err := s.regions.AppRegion(ctx, d.App.OwnerID, d.App.Name)
	if err != nil {
		s.log.Error("read app region", slog.String("app", d.App.Name), slog.String("error", err.Error()))
		return
	}
	rd := &AppRegionData{
		Name: name, Label: name, Recorded: d.App.Region != "",
		HasStorage: len(d.App.Volumes) > 0,
	}
	if rd.Label == "" {
		rd.Label = "not placed"
	}
	for _, o := range choice.Options {
		if o.Name == name {
			rd.Label = o.Label()
			continue
		}
		rd.Options = append(rd.Options, o)
	}
	d.Region = rd
}

// appRegionMove moves an app to another region.
func (s *Server) appRegionMove(w http.ResponseWriter, r *http.Request) {
	owner := identity.MustFromContext(r.Context())
	name := chi.URLParam(r, "name")
	back := "/apps/" + name + "/settings#region"
	region := strings.TrimSpace(r.FormValue("region"))

	err := s.regions.MoveApp(r.Context(), owner.ID, name, region)
	switch {
	case err == nil:
		s.flashOK(w, r, "Moving "+name+" to "+region+". It is redeployed there, and the old pods stop "+
			"once the new ones are ready.")
	case errors.Is(err, app.ErrMoveHasStorage), errors.Is(err, app.ErrRegionClosed),
		errors.Is(err, app.ErrNoSuchRegion), errors.Is(err, app.ErrCapacityFull),
		errors.Is(err, app.ErrNoRegions):
		s.flashErr(w, r, strings.TrimPrefix(err.Error(), "app: "))
	default:
		s.log.Error("move app region", slog.String("app", name), slog.String("error", err.Error()))
		s.flashErr(w, r, strings.TrimPrefix(err.Error(), "app: "))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
