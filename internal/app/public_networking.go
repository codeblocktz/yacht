package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/domain"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Railway's networking, as an app's settings show it: a public domain nobody
// gets until they ask, the address other apps reach it at, and a TCP proxy for
// what does not speak HTTP.

// DefaultEnvironment is the environment every app is in, until there are
// environments: what a generated hostname says after the app's name.
const DefaultEnvironment = "production"

// Environment is the environment the app is in: production, unless it is a
// service's copy in an environment made beside it.
func (a App) Environment() string {
	if a.EnvSlug == "" {
		return DefaultEnvironment
	}
	return a.EnvSlug
}

// ServiceName is what the canvas calls the app: its service, or its name for
// an app recorded before services were.
func (a App) ServiceName() string {
	if a.Service == "" {
		return a.Name
	}
	return a.Service
}

// ErrNoTCPProxyPort means every port TCP proxies may be given is in use.
var ErrNoTCPProxyPort = errors.New("app: every TCP proxy port on this install is in use")

// PublicDomain is the app's platform hostname, and what generating or changing
// one needs.
type PublicDomain struct {
	// Host is the platform hostname, empty when the app has none.
	Host string

	// Label is the part of Host before the domain, which is what the edit
	// form changes.
	Label string

	// Generated is a hostname under up.<app domain>. False with a Host is one
	// issued at <name>.<app domain> when every app was given one, which stays
	// where it is until somebody changes it.
	Generated bool

	// Routed is whether Host is on the Ingress now. False with a Host is an
	// app that has no port or has been made internal: it keeps the name, and
	// the page has to say why nothing answers on it.
	Routed bool

	// Domain is what a generated hostname goes under: up.<app domain>. Empty
	// when the install has no app domain, and nothing can be generated.
	Domain string

	// Suggested is the name Generate Domain gives: <app>-<environment>.
	Suggested string

	// Port is where the hostname's traffic goes: the app's port, one per app.
	// Suggestions are ports the app is known to listen on, the current first,
	// for the picker to offer.
	Port        int32
	Suggestions []int32

	// Unavailable says why Generate Domain cannot be offered, empty when it
	// can.
	Unavailable string
}

// PrivateEndpoint is where other apps in the cluster reach this one: its
// Service's name in cluster DNS, on the port that Service listens on. The same
// answer ${{app.PRIVATE_HOST}} and ${{app.PRIVATE_PORT}} resolve to.
type PrivateEndpoint struct {
	Host string
	Port int32
}

// TCPProxy is the app's public TCP port.
type TCPProxy struct {
	// Available is whether this install offers TCP proxies at all: it has a
	// name to reach them at.
	Available bool

	// Host and Port are what to connect to; Port is zero when the app has no
	// proxy. Target is the app's own port, where connections arrive.
	Host   string
	Port   int32
	Target int32
}

// Address is host:port, empty when there is no proxy.
func (p TCPProxy) Address() string {
	if p.Port == 0 {
		return ""
	}
	return p.Host + ":" + strconv.Itoa(int(p.Port))
}

// attachPublicNetworking fills in the parts of Networking this file owns.
func (s *Service) attachPublicNetworking(ctx context.Context, a App, out *Networking) error {
	held, ok, err := domain.GetManaged(ctx, s.q, a.ID)
	if err != nil {
		return err
	}
	pub := PublicDomain{
		Domain:      domain.GeneratedDomain(s.opts.AppDomain),
		Suggested:   domain.SuggestLabel(a.Name, a.Environment()),
		Port:        a.Port,
		Suggestions: portSuggestions(a),
		Unavailable: s.generateUnavailable(a),
	}
	if ok && s.opts.AppDomain != "" {
		pub.Host, pub.Label, pub.Generated = held.Host, held.Label, held.Generated
		pub.Routed = servesHTTP(a)
	}
	out.Public = pub
	out.Managed = pub.Host

	out.Private = PrivateEndpoint{
		Host: privateHost(a),
		Port: orchestrator.ServicePort(a.Internal, a.Port),
	}
	if a.Port == 0 {
		out.Private.Port = 0
	}

	out.TCPProxy = TCPProxy{
		Available: s.opts.TCPProxyHost != "",
		Host:      s.opts.TCPProxyHost,
		Target:    a.Port,
	}
	row, err := s.q.GetTCPProxy(ctx, dbgen.GetTCPProxyParams{OwnerID: a.OwnerID, AppID: a.ID})
	switch {
	case err == nil:
		out.TCPProxy.Port = row.Port
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("app: read tcp proxy: %w", err)
	}
	return nil
}

// privateHost is the app's Service in cluster DNS.
func privateHost(a App) string {
	return a.Name + "." + a.Namespace + ".svc.cluster.local"
}

// portSuggestions are the ports an app is known to listen on: the one it is
// configured with — for an app deployed from GitHub, what its Dockerfile
// exposes — then a PORT it sets for itself, then the port most frameworks
// take from PORT. Railway reads the port off the running process; Yacht has no
// way into the container to ask, so it offers what it was told.
func portSuggestions(a App) []int32 {
	var out []int32
	add := func(p int32) {
		if p > 0 && p <= 65535 && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	add(a.Port)
	for _, v := range a.Variables {
		if v.Key == "PORT" && !v.Secret && !v.Reference {
			if n, err := strconv.Atoi(v.Value); err == nil {
				add(int32(n))
			}
		}
	}
	add(DefaultPort)
	return out
}

// generateUnavailable says why an app cannot have a generated hostname, or ""
// when it can.
func (s *Service) generateUnavailable(a App) string {
	switch {
	case s.opts.AppDomain == "":
		return "This install has no app domain, so there is nothing to put a hostname under. " +
			"Whoever runs it sets YACHT_APP_DOMAIN."
	case a.Internal:
		return "This app is internal: it speaks something other than HTTP, on its own port. " +
			"Use a TCP proxy to reach it from outside, or make it public in its settings."
	}
	return ""
}

// GenerateDomain gives an app its public hostname: <app>-<environment> under
// up.<app domain>, or that with a few characters added when somebody already
// holds it. port, when not zero, becomes where its traffic goes, as Railway's
// Generate Domain asks which port to send to.
//
// An app that already has a platform hostname keeps it, and is told so,
// rather than having it replaced by pressing the button twice: changing the
// name is SetPublicDomain's, which is the form that says so.
func (s *Service) GenerateDomain(ctx context.Context, ownerID, name string, port int32) (string, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return "", err
	}
	if why := s.generateUnavailable(a); why != "" {
		return "", errors.New(why)
	}
	if _, ok, err := domain.GetManaged(ctx, s.q, a.ID); err != nil {
		return "", err
	} else if ok {
		return "", errors.New("this app already has a public domain; change it rather than generating another")
	}
	if a, err = s.targetPort(ctx, a, port); err != nil {
		return "", err
	}
	host, err := s.issueSuggested(ctx, a)
	if err != nil {
		return "", err
	}
	if err := s.routingChanged(ctx, a); err != nil {
		return "", err
	}
	s.log.Info("public domain generated", slog.String("app", a.Name), slog.String("host", host))
	return host, nil
}

// issueSuggested issues the suggested label, or the label with a random
// suffix while the name is taken. A handful of tries: four characters from 36
// make a collision after the first vanishingly unlikely, so running out means
// something other than bad luck is wrong.
func (s *Service) issueSuggested(ctx context.Context, a App) (string, error) {
	base := domain.SuggestLabel(a.Name, a.Environment())
	label := base
	for range 5 {
		host, err := domain.IssueGenerated(ctx, s.q, s.generateInput(a, label))
		if !errors.Is(err, domain.ErrHostTaken) {
			return host, err
		}
		label = domain.WithSuffix(base, randomSuffix())
	}
	return "", fmt.Errorf("%w: no free name like %s", domain.ErrHostTaken, base)
}

func (s *Service) generateInput(a App, label string) domain.GenerateInput {
	return domain.GenerateInput{
		OwnerID: a.OwnerID, AppID: a.ID, Label: label,
		AppDomain: s.opts.AppDomain, TLS: s.opts.WildcardTLS,
		Reserved: s.opts.ReservedDomains,
	}
}

// randomSuffix is four lowercase letters and digits, as Railway adds to a
// name somebody else has.
func randomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

// SetPublicDomain changes an app's public hostname to <label>.up.<app domain>,
// and where its traffic goes to port: Railway's "Update your domain or target
// port". A hostname issued at <name>.<app domain> moves under up.<app domain>
// once its name is changed, because only names under there are somebody's to
// choose; saved with its name as it was, it stays where it is, so changing the
// port of an app that has been live for months does not move its address.
//
// The old name is released in the same write, so nothing goes on holding a
// name the app no longer answers on; until the Ingress is rewritten a moment
// later, it is the old name that still routes.
func (s *Service) SetPublicDomain(ctx context.Context, ownerID, name, label string, port int32) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if why := s.generateUnavailable(a); why != "" {
		return errors.New(why)
	}
	held, ok, err := domain.GetManaged(ctx, s.q, a.ID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("this app has no public domain to change; generate one first")
	}
	if err := domain.ValidateLabel(label); err != nil {
		return err
	}
	if a, err = s.targetPort(ctx, a, port); err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(label)) == held.Label {
		return nil
	}
	host, err := domain.IssueGenerated(ctx, s.q, s.generateInput(a, label))
	if err != nil {
		return err
	}
	if host == held.Host {
		return nil
	}
	if err := s.routingChanged(ctx, a); err != nil {
		return err
	}
	s.log.Info("public domain changed", slog.String("app", a.Name),
		slog.String("from", held.Host), slog.String("to", host))
	return nil
}

// RemovePublicDomain takes the app's platform hostname away and releases the
// name. Generating again later gives a generated one, not the old
// <name>.<app domain> an app may have been issued once.
func (s *Service) RemovePublicDomain(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if err := domain.RetireManaged(ctx, s.q, a.ID); err != nil {
		return err
	}
	if err := s.routingChanged(ctx, a); err != nil {
		return err
	}
	s.log.Info("public domain removed", slog.String("app", a.Name))
	return nil
}

// targetPort makes port the app's port, when it is a change: one port per app,
// which its Service sends to and every hostname routes to. Zero leaves the
// port as it is. A change rolls the app out, as changing it in Settings does —
// the container is told its PORT, and the Service follows.
func (s *Service) targetPort(ctx context.Context, a App, port int32) (App, error) {
	if port == 0 {
		if a.Port == 0 {
			return a, errors.New("choose the port your app listens on, so its traffic has somewhere to go")
		}
		return a, nil
	}
	if port < 1 || port > 65535 {
		return a, errors.New("the port must be between 1 and 65535")
	}
	if port == a.Port {
		return a, nil
	}
	updated, err := s.Update(ctx, a.OwnerID, a.Name, UpdateInput{
		Image: a.Image, Port: port, Internal: a.Internal, Repo: a.Repo,
		CPURequest: a.CPURequest, CPULimit: a.CPULimit,
		MemoryRequest: a.MemoryRequest, MemoryLimit: a.MemoryLimit,
	})
	if err != nil {
		return a, err
	}
	updated.Variables, updated.Volumes = a.Variables, a.Volumes
	return updated, nil
}

// routingChanged has the app reconciler rewrite the app's routing, the way a
// custom domain being verified does: a new convergence key for it to observe,
// and the route observer told.
func (s *Service) routingChanged(ctx context.Context, a App) error {
	if _, err := s.q.IncrementAppConfigVersion(ctx, dbgen.IncrementAppConfigVersionParams{
		OwnerID: a.OwnerID, AppID: a.ID,
	}); err != nil {
		return fmt.Errorf("app: version routing change: %w", err)
	}
	s.notifyRoutes(RouteChange{Kind: RouteChangeApp, OwnerID: a.OwnerID, AppID: a.ID, App: a.Name})
	return nil
}

// EnableTCPProxy gives an app a public TCP port, reaching its own port from
// every machine. An app that has one keeps it: the port is what somebody's
// connection string says.
func (s *Service) EnableTCPProxy(ctx context.Context, ownerID, name string) (TCPProxy, error) {
	if s.opts.TCPProxyHost == "" {
		return TCPProxy{}, errors.New("this install does not offer TCP proxies: whoever runs it sets YACHT_TCP_PROXY_HOST")
	}
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return TCPProxy{}, err
	}
	if a.Port == 0 {
		return TCPProxy{}, errors.New("this app has no port for a TCP proxy to send connections to; set one in its settings")
	}
	port, err := s.claimTCPProxyPort(ctx, a)
	if err != nil {
		return TCPProxy{}, err
	}
	if err := s.routingChanged(ctx, a); err != nil {
		return TCPProxy{}, err
	}
	s.log.Info("tcp proxy enabled", slog.String("app", a.Name), slog.Int("port", int(port)))
	return TCPProxy{Available: true, Host: s.opts.TCPProxyHost, Port: port, Target: a.Port}, nil
}

// claimTCPProxyPort picks a free port from the install's range and records it
// as the app's. At random rather than the lowest free, so a port given up is
// not the next one handed out, to whoever asks next, while somebody's old
// connection string still names it. The unique index decides a race between
// two apps picking the same port, and the loser picks again.
func (s *Service) claimTCPProxyPort(ctx context.Context, a App) (int32, error) {
	first, last := s.opts.TCPProxyPortFirst, s.opts.TCPProxyPortLast
	if first == 0 || last < first {
		return 0, errors.New("app: no TCP proxy ports are configured")
	}
	for range 5 {
		used, err := s.q.ListTCPProxyPorts(ctx)
		if err != nil {
			return 0, fmt.Errorf("app: list tcp proxy ports: %w", err)
		}
		var free []int32
		for p := first; p <= last; p++ {
			if _, taken := slices.BinarySearch(used, p); !taken {
				free = append(free, p)
			}
		}
		if len(free) == 0 {
			return 0, ErrNoTCPProxyPort
		}
		row, err := s.q.CreateTCPProxy(ctx, dbgen.CreateTCPProxyParams{
			OwnerID: a.OwnerID, AppID: a.ID, Port: free[rand.IntN(len(free))],
		})
		if err == nil {
			return row.Port, nil
		}
		if !isUniqueViolation(err) {
			return 0, fmt.Errorf("app: record tcp proxy: %w", err)
		}
	}
	return 0, ErrNoTCPProxyPort
}

// DisableTCPProxy takes the app's TCP port off every machine.
func (s *Service) DisableTCPProxy(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if _, err := s.q.DeleteTCPProxy(ctx, dbgen.DeleteTCPProxyParams{
		OwnerID: ownerID, AppID: a.ID,
	}); err != nil {
		return fmt.Errorf("app: remove tcp proxy: %w", err)
	}
	if err := s.routingChanged(ctx, a); err != nil {
		return err
	}
	s.log.Info("tcp proxy disabled", slog.String("app", a.Name))
	return nil
}

// tcpProxyPortFor is the proxy port an apply hands the cluster: the app's
// recorded one, while the install offers proxies and the app has a port for
// it to reach.
//
// A read that fails fails the apply, rather than being taken for no proxy:
// an apply with none removes the one in the cluster, and nothing would put it
// back until the app next changed, while the apply failing is retried.
func (s *Service) tcpProxyPortFor(ctx context.Context, q *dbgen.Queries, a App) (int32, error) {
	if s.opts.TCPProxyHost == "" || a.Port == 0 {
		return 0, nil
	}
	row, err := q.GetTCPProxyByApp(ctx, a.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("app: read tcp proxy: %w", err)
	}
	return row.Port, nil
}
