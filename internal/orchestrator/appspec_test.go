package orchestrator

import "testing"

func validSpec() AppSpec {
	return AppSpec{
		Ref:      Ref{Owner: "owner-1", Namespace: "yacht-demo", Name: "web"},
		Image:    "nginx:alpine",
		Replicas: 1,
		Port:     8080,
	}
}

func TestAppSpecAcceptsHosts(t *testing.T) {
	s := validSpec()
	s.Hosts = []string{"web.apps.example.com", "www.customer.test"}
	s.TLS = true
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestAppSpecRejectsMalformedHosts(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"space":         "web .example.com",
		"uppercase":     "WEB.example.com",
		"scheme":        "https://web.example.com",
		"path":          "web.example.com/app",
		"port":          "web.example.com:8080",
		"leading dot":   ".web.example.com",
		"double dot":    "web..example.com",
		"underscore":    "web_1.example.com",
		"trailing dash": "web-.example.com",
	}

	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.Hosts = []string{host}
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted malformed host %q", host)
			}
		})
	}
}

// A spec that takes no traffic cannot be routed to, so declaring hosts for it
// is a wiring mistake worth catching here rather than producing an Ingress
// pointing at a Service that was never created.
func TestAppSpecRejectsHostsWithoutAPort(t *testing.T) {
	s := validSpec()
	s.Port = 0
	s.Hosts = []string{"web.apps.example.com"}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted hosts on a spec with no port")
	}
}

func TestAppSpecAcceptsVolumes(t *testing.T) {
	s := validSpec()
	s.Volumes = []VolumeSpec{{Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30}}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A ReadWriteOnce volume mounts on one node at a time, so the second pod can
// never schedule. Refusing here means the person is told at the moment they
// ask; the alternative is a pod stuck Pending forever with the reason buried
// in kubectl describe.
func TestAppSpecRefusesVolumesWithMoreThanOneReplica(t *testing.T) {
	s := validSpec()
	s.Replicas = 2
	s.Volumes = []VolumeSpec{{Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30}}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a volume on a workload with two replicas")
	}
}

func TestAppSpecRejectsMalformedVolumes(t *testing.T) {
	cases := map[string]VolumeSpec{
		"no name":         {MountPath: "/data", SizeBytes: 1 << 30},
		"bad name":        {Name: "Data!", MountPath: "/data", SizeBytes: 1 << 30},
		"relative mount":  {Name: "data", MountPath: "var/lib", SizeBytes: 1 << 30},
		"root mount":      {Name: "data", MountPath: "/", SizeBytes: 1 << 30},
		"no size":         {Name: "data", MountPath: "/data"},
		"negative size":   {Name: "data", MountPath: "/data", SizeBytes: -1},
		"trailing slash":  {Name: "data", MountPath: "/data/", SizeBytes: 1 << 30},
		"dot dot in path": {Name: "data", MountPath: "/data/../etc", SizeBytes: 1 << 30},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.Volumes = []VolumeSpec{v}
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", v)
			}
		})
	}
}

// Two volumes at one path is a workload where one silently wins.
func TestAppSpecRejectsDuplicateMountPaths(t *testing.T) {
	s := validSpec()
	s.Volumes = []VolumeSpec{
		{Name: "a", MountPath: "/var/lib/data", SizeBytes: 1 << 30},
		{Name: "b", MountPath: "/var/lib/data", SizeBytes: 1 << 30},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted two volumes mounted at the same path")
	}
}

func TestAppSpecAcceptsAHealthPath(t *testing.T) {
	s := validSpec()
	s.HealthPath = "/healthz"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A probe is an HTTP GET against the app's own port. With no port there is
// nothing to probe, and Kubernetes would reject the pod rather than explain.
func TestAppSpecRejectsAHealthPathWithNoPort(t *testing.T) {
	s := validSpec()
	s.Port = 0
	s.HealthPath = "/healthz"
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a health path on a workload with no port")
	}
}

func TestAppSpecRejectsMalformedHealthPaths(t *testing.T) {
	for name, path := range map[string]string{
		"relative":    "healthz",
		"scheme":      "http://x/healthz",
		"space":       "/health z",
		"not clean":   "/a/../healthz",
		"query":       "/healthz?x=1",
	} {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.HealthPath = path
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted %q", path)
			}
		})
	}
}

// Liveness restarts the container. Asking for it without saying what to probe
// is a request that cannot be honoured, and honouring it by guessing would
// restart a working app.
func TestAppSpecRejectsLivenessWithNoHealthPath(t *testing.T) {
	s := validSpec()
	s.Liveness = true
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted liveness with nothing to probe")
	}
}
