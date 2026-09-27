package config

import (
	"strings"
	"testing"
	"time"
)

func TestTheWakerIsOffUntilItHasAnAddress(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WakerAddr != "" || c.WakerListenAddr() != "" || c.WakeTimeout != 60*time.Second {
		t.Fatalf("waker = %q listening on %q, timeout %v; want off, 60s",
			c.WakerAddr, c.WakerListenAddr(), c.WakeTimeout)
	}

	setEnv(t, map[string]string{"YACHT_WAKER_ADDR": "10.0.0.5:8090", "YACHT_WAKE_TIMEOUT": "90s"})
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ip, port, err := c.Waker()
	if err != nil || ip != "10.0.0.5" || port != 8090 || c.WakerListenAddr() != ":8090" ||
		c.WakeTimeout != 90*time.Second {
		t.Fatalf("waker = %s:%d (%v) listening on %q, timeout %v",
			ip, port, err, c.WakerListenAddr(), c.WakeTimeout)
	}
}

func TestAWakerTheClusterCannotReachFailsAtStartup(t *testing.T) {
	for _, addr := range []string{
		"localhost:8090", "127.0.0.1:8090", "0.0.0.0:8090", "10.0.0.5", "10.0.0.5:0", "yacht.internal:8090",
	} {
		setEnv(t, map[string]string{"YACHT_WAKER_ADDR": addr})
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YACHT_WAKER_ADDR") {
			t.Errorf("YACHT_WAKER_ADDR=%s loaded: %v", addr, err)
		}
	}
	setEnv(t, map[string]string{"YACHT_WAKER_ADDR": "", "YACHT_WAKER_LISTEN": ":8090"})
	if _, err := Load(); err == nil {
		t.Errorf("a waker listening at no address the cluster knows loaded")
	}
	setEnv(t, map[string]string{"YACHT_WAKER_LISTEN": "", "YACHT_WAKE_TIMEOUT": "1s"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "YACHT_WAKE_TIMEOUT") {
		t.Errorf("a one-second wake timeout loaded: %v", err)
	}
}
