package notify

import "testing"

func TestNewSMTPRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]SMTPConfig{
		"no address": {From: "yacht@example.test"},
		"no from":    {Addr: "smtp.example.test:587"},
		"host only":  {Addr: "smtp.example.test", From: "yacht@example.test"},
		"password only": {
			Addr: "smtp.example.test:587", From: "yacht@example.test",
			Password: "secret",
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSMTP(cfg); err == nil {
				t.Fatalf("NewSMTP accepted %+v", cfg)
			}
		})
	}
}

func TestNewSMTPAcceptsCompleteConfig(t *testing.T) {
	for name, cfg := range map[string]SMTPConfig{
		"anonymous": {Addr: "smtp.example.test:587", From: "yacht@example.test"},
		"authenticated": {
			Addr: "smtp.example.test:587", From: "yacht@example.test",
			Username: "user", Password: "secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSMTP(cfg); err != nil {
				t.Fatalf("NewSMTP: %v", err)
			}
		})
	}
}
