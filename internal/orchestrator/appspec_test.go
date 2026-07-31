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
