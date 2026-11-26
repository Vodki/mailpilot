package claudecli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/llm"
)

const schema = `{"type":"object","properties":{"status":{"type":"string"}},"required":["status"],"additionalProperties":false}`

func newFake(t *testing.T, mode string, timeout time.Duration) (*Provider, string) {
	t.Helper()
	bin, err := filepath.Abs("testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("FAKE_MODE", mode)
	t.Setenv("FAKE_ARGS_FILE", filepath.Join(dir, "args"))
	t.Setenv("FAKE_STDIN_FILE", filepath.Join(dir, "stdin"))
	t.Setenv("FAKE_ENV_FILE", filepath.Join(dir, "env"))
	t.Setenv("MAILPILOT_IMAP_PASSWORD", "must-not-leak")
	p := New(config.ClaudeCLIConfig{Binary: bin, Timeout: timeout, MaxConcurrency: 1},
		map[string]config.LLMTask{"triage": {Model: "claude-opus-5-5", Effort: "low", MaxBudgetUSD: 0.5}})
	// FAKE_* are not in the passthrough list; add them for the script only.
	for _, k := range []string{"FAKE_MODE", "FAKE_ARGS_FILE", "FAKE_STDIN_FILE", "FAKE_ENV_FILE"} {
		p.env = append(p.env, k+"="+os.Getenv(k))
	}
	return p, dir
}

func req(prompt string) llm.Request {
	return llm.Request{Task: "triage", System: "sys", Prompt: prompt, Schema: []byte(schema)}
}

func TestCompleteSuccess(t *testing.T) {
	p, dir := newFake(t, "ok", 10*time.Second)
	prompt := `<email untrusted="true">secret body</email>`
	res, err := p.Complete(context.Background(), req(prompt))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != `{"status":"ok"}` || res.CostUSD != 0.0123 || res.Usage.InputTokens != 100 || res.Model != "claude-opus-5-5" {
		t.Fatalf("unexpected response %+v", res)
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	lines := strings.Split(strings.TrimRight(string(args), "\n"), "\n")
	for _, want := range []string{"-p", "--bare", "--strict-mcp-config", "--no-session-persistence"} {
		if !slices.Contains(lines, want) {
			t.Errorf("missing flag %s in %q", want, lines)
		}
	}
	if i := slices.Index(lines, "--tools"); i < 0 || lines[i+1] != "" {
		t.Errorf(`--tools "" missing: %q`, lines)
	}
	if strings.Contains(string(args), "secret body") {
		t.Error("email content must not appear on the command line")
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(stdin) != prompt {
		t.Errorf("prompt not passed on stdin: %q", stdin)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(env), "must-not-leak") {
		t.Error("child process inherited mailpilot secrets")
	}
}

func TestRefusesExposedTools(t *testing.T) {
	for _, mode := range []string{"tools", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := newFake(t, mode, 10*time.Second)
			start := time.Now()
			_, err := p.Complete(context.Background(), req("x"))
			if !errors.Is(err, llm.ErrToolsExposed) {
				t.Fatalf("expected ErrToolsExposed, got %v", err)
			}
			if time.Since(start) > 4*time.Second {
				t.Fatal("process was not killed promptly")
			}
		})
	}
}

func TestErrorResult(t *testing.T) {
	p, _ := newFake(t, "error", 10*time.Second)
	_, err := p.Complete(context.Background(), req("x"))
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestTimeoutKillsProcess(t *testing.T) {
	p, _ := newFake(t, "hang", 500*time.Millisecond)
	start := time.Now()
	if _, err := p.Complete(context.Background(), req("x")); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: %s", time.Since(start))
	}
}

func TestVersion(t *testing.T) {
	p, _ := newFake(t, "ok", time.Second)
	v, err := p.Version(context.Background())
	if err != nil || !strings.Contains(v, "Claude Code") {
		t.Fatalf("version %q err %v", v, err)
	}
}

func TestUnknownTask(t *testing.T) {
	p, _ := newFake(t, "ok", time.Second)
	r := req("x")
	r.Task = "nope"
	if _, err := p.Complete(context.Background(), r); err == nil {
		t.Fatal("expected error")
	}
}
