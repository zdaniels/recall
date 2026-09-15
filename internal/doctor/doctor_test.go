package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckHermes(t *testing.T) {
	// os.UserHomeDir() honours $HOME on unix; point it at a temp dir per case.
	t.Run("not installed -> omitted", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if _, ok := checkHermes(); ok {
			t.Fatalf("expected check omitted when ~/.hermes absent")
		}
	})

	t.Run("installed, no config -> warn", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
			t.Fatal(err)
		}
		c, ok := checkHermes()
		if !ok || c.Status != Warn {
			t.Fatalf("want warn, got ok=%v status=%v", ok, c.Status)
		}
	})

	t.Run("config without entry -> warn", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHermesCfg(t, home, "mcp_servers:\n  time:\n    command: uvx\n    args: [\"mcp-server-time\"]\n")
		c, ok := checkHermes()
		if !ok || c.Status != Warn {
			t.Fatalf("want warn, got ok=%v status=%v", ok, c.Status)
		}
	})

	t.Run("config with recall -> pass", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeHermesCfg(t, home, "mcp_servers:\n  recall:\n    command: /x/recall\n    args: [mcp]\n")
		c, ok := checkHermes()
		if !ok || c.Status != Pass {
			t.Fatalf("want pass, got ok=%v status=%v detail=%q", ok, c.Status, c.Detail)
		}
	})
}

func writeHermesCfg(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".hermes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
