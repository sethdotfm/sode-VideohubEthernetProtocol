package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func nolog(string, ...any) {}

func TestOpenHolderSeedsFromEnv(t *testing.T) {
	t.Setenv("VEP_HUB", " 10.0.0.5 ")
	t.Setenv("VEP_PROTECT_DEVICE", "7")
	p := filepath.Join(t.TempDir(), "config.yaml")
	var msg string
	h, err := OpenHolder(p, func(f string, a ...any) { msg = f })
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get(); got.Hub != "10.0.0.5" || got.ProtectDevice != 7 {
		t.Fatalf("got %+v", got)
	}
	if _, exists, err := Load(p); err != nil || !exists {
		t.Fatalf("file not created: exists=%v err=%v", exists, err)
	}
	if !strings.Contains(msg, "created") {
		t.Fatalf("log %q", msg)
	}
}

func TestOpenHolderNoPath(t *testing.T) {
	t.Setenv("VEP_HUB", "h")
	h, err := OpenHolder("", nolog)
	if err != nil || h.Get().Hub != "h" {
		t.Fatalf("%v %+v", err, h)
	}
	if _, err := h.Set(Config{Hub: "x", ProtectDevice: 1, ResyncSeconds: 60}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenHolderFileWinsOverEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("hub: fromfile\nprotectDevice: 3\nresyncSeconds: 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VEP_HUB", "fromenv")
	h, err := OpenHolder(p, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get(); got.Hub != "fromfile" || got.ProtectDevice != 3 || got.ResyncSeconds != 30 {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenHolderBadFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("nope: 1\n"), 0o644)
	if _, err := OpenHolder(p, nolog); err == nil {
		t.Fatal("want error")
	}
}

func TestOldDebugFileLoadsAndIsNotRewritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("hub: a\nprotectDevice: 1\nresyncSeconds: 60\ndebug: true\n"), 0o644)
	h, err := OpenHolder(p, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Get().Debug {
		t.Fatal("debug should load")
	}
	if _, err := h.Set(Config{Hub: "b", ProtectDevice: 1, ResyncSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "debug") {
		t.Fatalf("debug written:\n%s", b)
	}
}

func TestSetInvalidLeavesEverythingAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("hub: a\nprotectDevice: 1\nresyncSeconds: 60\n"), 0o644)
	before, _ := os.ReadFile(p)
	h, _ := OpenHolder(p, nolog)
	old := h.Get()
	changed, err := h.Set(Config{Hub: "b", ProtectDevice: 1, ResyncSeconds: 1})
	if err == nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if h.Get() != old {
		t.Fatal("config changed")
	}
	after, _ := os.ReadFile(p)
	if string(after) != string(before) {
		t.Fatal("file changed")
	}
}

func TestSetSaveFailureKeepsOld(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	os.WriteFile(p, []byte("hub: a\nprotectDevice: 1\nresyncSeconds: 60\n"), 0o644)
	h, err := OpenHolder(p, nolog)
	if err != nil {
		t.Fatal(err)
	}
	old := h.Get()
	h.path = filepath.Join(dir, "missing", "config.yaml") // directory does not exist
	changed, err := h.Set(Config{Hub: "b", ProtectDevice: 1, ResyncSeconds: 60})
	if err == nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if h.Get() != old {
		t.Fatal("config changed after failed save")
	}
}

func TestSetReportsHubChangeOnly(t *testing.T) {
	h, _ := OpenHolder("", nolog)
	c := Config{Hub: "a", ProtectDevice: 1, ResyncSeconds: 60}
	if changed, err := h.Set(c); err != nil || !changed {
		t.Fatalf("hub change: %v %v", changed, err)
	}
	c.ProtectDevice = 2
	if changed, err := h.Set(c); err != nil || changed {
		t.Fatalf("other field: %v %v", changed, err)
	}
	if h.Get().ProtectDevice != 2 {
		t.Fatal("not applied")
	}
}

func TestHolderConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	h, err := OpenHolder(p, nolog)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if g%2 == 0 {
					h.Set(Config{Hub: "h", ProtectDevice: g, ResyncSeconds: 60 + i})
				} else {
					_ = h.Get()
				}
			}
		}(g)
	}
	wg.Wait()
	if _, exists, err := Load(p); err != nil || !exists {
		t.Fatalf("file: %v %v", exists, err)
	}
}
