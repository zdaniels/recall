// Package doctor implements `recall doctor` — a self-check that verifies
// every external surface recall depends on (binary location, PATH,
// DB health, Claude Code hooks + MCP registration, CLAUDE.md guidance).
//
// Output is pass / warn / fail per check. Warnings represent recoverable
// issues (missing CLAUDE.md, log dir absent); failures mean recall is
// not actually wired into the active environment.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/zdaniels/recall/internal/embed"
	"github.com/zdaniels/recall/internal/store"
)

type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

type Check struct {
	Name   string
	Status Status
	Detail string
}

type Report struct {
	Checks []Check
}

func (r *Report) add(c Check) { r.Checks = append(r.Checks, c) }

func (r Report) HasFailures() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

func (r Report) Print(w io.Writer) {
	for _, c := range r.Checks {
		sym := "✓"
		switch c.Status {
		case Warn:
			sym = "⚠"
		case Fail:
			sym = "✗"
		}
		fmt.Fprintf(w, "  %s %-32s", sym, c.Name)
		if c.Detail != "" {
			fmt.Fprintf(w, " %s", c.Detail)
		}
		fmt.Fprintln(w)
	}
}

// Run executes all checks in order.
func Run(ctx context.Context, dbPath string) Report {
	var r Report
	r.add(checkBinary())
	r.add(checkPATH())
	r.add(checkDB(dbPath))
	r.add(checkContents(ctx, dbPath))
	r.add(checkOnnxAssets())
	r.add(checkClaudeSettings())
	r.add(checkClaudeMD())
	// Hermes is only probed when it's actually installed (~/.hermes present),
	// so users who don't run Hermes see no noise.
	if c, ok := checkHermes(); ok {
		r.add(c)
	}
	r.add(checkLogDir())
	return r
}

// hermesRecallEntry matches a `recall:` key indented under a parent
// (i.e. an mcp_servers entry) in ~/.hermes/config.yaml. A deliberately light
// textual check — we don't pull in a YAML dependency just for a doctor hint.
var hermesRecallEntry = regexp.MustCompile(`(?m)^[ \t]+recall:`)

// checkHermes reports whether recall's MCP server is registered in Hermes.
// The bool is false when Hermes isn't installed, so the caller can omit the
// check entirely rather than show an irrelevant warning.
func checkHermes() (Check, bool) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".hermes")
	if _, err := os.Stat(dir); err != nil {
		return Check{}, false // Hermes not installed
	}
	cfg := filepath.Join(dir, "config.yaml")
	b, err := os.ReadFile(cfg)
	if err != nil {
		return Check{
			Name:   "Hermes wiring",
			Status: Warn,
			Detail: "~/.hermes present but config.yaml missing — run Hermes once, then `make setup`",
		}, true
	}
	text := string(b)
	if !strings.Contains(text, "mcp_servers:") || !hermesRecallEntry.MatchString(text) {
		return Check{
			Name:   "Hermes wiring",
			Status: Warn,
			Detail: "recall not in ~/.hermes/config.yaml mcp_servers — run `make setup` to add it",
		}, true
	}
	return Check{Name: "Hermes wiring", Status: Pass, Detail: "recall MCP server registered"}, true
}

func checkOnnxAssets() Check {
	// Use the embed package's OS-aware paths so this probe matches what
	// the loader actually looks for (.dylib on macOS, .so on Linux).
	paths := map[string]string{
		"runtime":   embed.RuntimeLibPath(),
		"model":     embed.ModelPath(),
		"tokenizer": embed.TokenizerPath(),
	}
	missing := []string{}
	for name, p := range paths {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// The Apple NLEmbedding fallback only exists on darwin; elsewhere
		// the fallback is Ollama.
		fallback := "Ollama fallback active"
		if runtime.GOOS == "darwin" {
			fallback = "Apple NLEmbedding fallback active"
		}
		return Check{
			Name:   "ONNX semantic recall",
			Status: Warn,
			Detail: fmt.Sprintf("missing: %s — run `recall download-model` (%s)", strings.Join(missing, ", "), fallback),
		}
	}
	return Check{Name: "ONNX semantic recall", Status: Pass, Detail: "all-MiniLM-L6-v2 ready (q8)"}
}

func checkBinary() Check {
	exe, err := os.Executable()
	if err != nil {
		return Check{Name: "binary path", Status: Warn, Detail: err.Error()}
	}
	st, err := os.Stat(exe)
	if err != nil {
		return Check{Name: "binary path", Status: Fail, Detail: err.Error()}
	}
	return Check{
		Name:   "binary",
		Status: Pass,
		Detail: fmt.Sprintf("%s (%s)", exe, humanBytes(st.Size())),
	}
}

func checkPATH() Check {
	if _, err := exec.LookPath("recall"); err != nil {
		return Check{
			Name:   "recall on PATH",
			Status: Warn,
			Detail: "hooks may fail; consider adding ~/.local/bin to PATH",
		}
	}
	return Check{Name: "recall on PATH", Status: Pass}
}

func checkDB(path string) Check {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return Check{Name: "database file", Status: Warn, Detail: "not yet created — run `recall ingest`"}
	}
	if err != nil {
		return Check{Name: "database file", Status: Fail, Detail: err.Error()}
	}
	return Check{
		Name:   "database file",
		Status: Pass,
		Detail: fmt.Sprintf("%s (%s)", path, humanBytes(st.Size())),
	}
}

func checkContents(ctx context.Context, path string) Check {
	if _, err := os.Stat(path); err != nil {
		return Check{Name: "database contents", Status: Warn, Detail: "(skipped — db missing)"}
	}
	st, err := store.Open(path)
	if err != nil {
		return Check{Name: "database contents", Status: Fail, Detail: err.Error()}
	}
	defer st.Close()
	stats, err := st.Stats()
	if err != nil {
		return Check{Name: "database contents", Status: Fail, Detail: err.Error()}
	}
	var decCount int
	_ = st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE superseded_by IS NULL`).Scan(&decCount)
	return Check{
		Name:   "database contents",
		Status: Pass,
		Detail: fmt.Sprintf("sessions=%d turns=%d files=%d decisions=%d projects=%d",
			stats.Sessions, stats.Turns, stats.Files, decCount, stats.Projects),
	}
}

func checkClaudeSettings() Check {
	home, _ := os.UserHomeDir()
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return Check{Name: "Claude Code settings.json", Status: Warn, Detail: "not found — hooks won't fire"}
	}
	var cfg struct {
		Hooks map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Check{Name: "Claude Code settings.json", Status: Fail, Detail: "invalid JSON: " + err.Error()}
	}
	required := []string{"SessionStart", "Stop"}
	optional := []string{"UserPromptSubmit", "PostToolUse"}
	var missing, present []string
	for _, h := range required {
		if _, ok := cfg.Hooks[h]; !ok {
			missing = append(missing, h+" hook")
		} else {
			present = append(present, h)
		}
	}
	for _, h := range optional {
		if _, ok := cfg.Hooks[h]; ok {
			present = append(present, h)
		}
	}

	// Claude Code 2.1+ stores MCP server registrations in ~/.claude.json
	// (not in ~/.claude/settings.json's `mcpServers` block, which is silently
	// ignored). Look there for the recall entry.
	mcpRegistered := false
	if data, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var dot struct {
			MCPServers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(data, &dot) == nil {
			_, mcpRegistered = dot.MCPServers["recall"]
		}
	}
	if !mcpRegistered {
		missing = append(missing, "recall MCP server (run: claude mcp add --scope user recall ~/.local/bin/recall mcp)")
	}

	if len(missing) > 0 {
		return Check{
			Name:   "Claude Code wiring",
			Status: Warn,
			Detail: "missing: " + strings.Join(missing, ", "),
		}
	}
	return Check{
		Name:   "Claude Code wiring",
		Status: Pass,
		Detail: "hooks (" + strings.Join(present, ", ") + ") + MCP server registered",
	}
}

func checkClaudeMD() Check {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude", "CLAUDE.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return Check{
			Name:   "global CLAUDE.md",
			Status: Warn,
			Detail: "missing — agent won't know recall_* tools exist",
		}
	}
	if !strings.Contains(strings.ToLower(string(b)), "recall") {
		return Check{
			Name:   "global CLAUDE.md",
			Status: Warn,
			Detail: "exists but doesn't mention `recall`",
		}
	}
	return Check{Name: "global CLAUDE.md", Status: Pass, Detail: "references recall"}
}

func checkLogDir() Check {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".local", "state", "recall")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return Check{
			Name:   "log dir",
			Status: Warn,
			Detail: "will be created on first hook fire",
		}
	}
	return Check{Name: "log dir", Status: Pass, Detail: dir}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
