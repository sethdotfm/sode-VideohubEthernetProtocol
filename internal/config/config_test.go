package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubAddrDefaultsPort(t *testing.T) {
	for in, want := range map[string]string{"": "", "10.0.0.5": "10.0.0.5:9990", "10.0.0.5:1234": "10.0.0.5:1234", "::1": "[::1]:9990"} {
		if got := (Config{Hub: in}).HubAddr(); got != want {
			t.Errorf("HubAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.ResyncSeconds = 1
	if c.Validate() == nil {
		t.Error("resync 1 accepted")
	}
	c = Defaults()
	c.Hub = "host:99999"
	if c.Validate() == nil {
		t.Error("bad port accepted")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if _, exists, err := Load(p); exists || err != nil {
		t.Fatalf("missing file: exists=%v err=%v", exists, err)
	}
	want := Defaults()
	want.Hub, want.ProtectDevice = "10.1.2.3", 7
	if err := Save(p, want); err != nil {
		t.Fatal(err)
	}
	got, exists, err := Load(p)
	if !exists || err != nil || got != want {
		t.Fatalf("got %+v exists=%v err=%v", got, exists, err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("VEP_HUB", "hub.local")
	t.Setenv("VEP_PROTECT_DEVICE", "3")
	c, err := FromEnv(Defaults())
	if err != nil || c.Hub != "hub.local" || c.ProtectDevice != 3 {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("VEP_RESYNC_SECONDS", "x")
	if _, err := FromEnv(Defaults()); err == nil {
		t.Error("bad int accepted")
	}
}

func TestOldFileWithDebugStillLoadsAndIsNotSaved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("hub: 10.0.0.9\nprotectDevice: 1\nresyncSeconds: 60\ndebug: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, exists, err := Load(p)
	if err != nil || !exists || c.Hub != "10.0.0.9" {
		t.Fatalf("%+v %v %v", c, exists, err)
	}
	c.Debug = false
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "debug") {
		t.Fatalf("debug was saved: %s", b)
	}
}

func TestDebugFromEnv(t *testing.T) {
	t.Setenv("VEP_DEBUG", "true")
	if !DebugFromEnv() {
		t.Error("true not seen")
	}
	t.Setenv("VEP_DEBUG", "")
	if DebugFromEnv() {
		t.Error("empty is on")
	}
}
