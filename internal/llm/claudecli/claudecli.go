// Package claudecli implements llm.Provider on top of `claude -p` (Claude
// Code in print mode). Every call starts a fresh process with all tools, MCP
// servers, hooks, plugins and local settings disabled, feeds the prompt on
// stdin and reads a stream-json transcript.
package claudecli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/llm"
)

// allowedTools is what a print-mode session may report in its init event.
// StructuredOutput is the internal tool Claude Code uses to return the
// --json-schema result; it has no side effect.
var allowedTools = []string{"StructuredOutput"}

// passthroughEnv lists the only variables the child process inherits.
var passthroughEnv = []string{
	"PATH", "HOME", "TMPDIR", "LANG",
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
	"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS",
}

// Provider runs `claude -p`.
type Provider struct {
	cfg   config.ClaudeCLIConfig
	tasks map[string]config.LLMTask
	sem   chan struct{}
	env   []string
}

// New builds the provider from configuration.
func New(cfg config.ClaudeCLIConfig, tasks map[string]config.LLMTask) *Provider {
	n := cfg.MaxConcurrency
	if n < 1 {
		n = 1
	}
	return &Provider{cfg: cfg, tasks: tasks, sem: make(chan struct{}, n), env: childEnv(os.LookupEnv)}
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return "claude-cli" }

func childEnv(lookup func(string) (string, bool)) []string {
	env := []string{
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	for _, k := range passthroughEnv {
		if v, ok := lookup(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Args returns the fixed command line for a task. Exported for tests and for
// `mailpilot check`, which prints it.
func Args(task config.LLMTask, system string, schema json.RawMessage) []string {
	args := []string{
		"-p",
		"--bare",
		"--tools", "",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose", // required by stream-json in print mode
		"--json-schema", string(schema),
		"--system-prompt", system,
		"--model", task.Model,
	}
	if task.Effort != "" {
		args = append(args, "--effort", task.Effort)
	}
	if task.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(task.MaxBudgetUSD, 'f', -1, 64))
	}
	return args
}

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	task, ok := p.tasks[req.Task]
	if !ok {
		return nil, fmt.Errorf("claudecli: unknown task %q", req.Task)
	}
	if !json.Valid(req.Schema) {
		return nil, errors.New("claudecli: schema is not valid JSON")
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.sem }()

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	workdir, err := os.MkdirTemp("", "mailpilot-llm-")
	if err != nil {
		return nil, fmt.Errorf("claudecli: workdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workdir) }()

	start := time.Now()
	cmd := exec.CommandContext(ctx, p.cfg.Binary, Args(task, req.System, req.Schema)...)
	cmd.Dir = workdir
	cmd.Env = p.env
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{buf: &stderr, max: 4096}
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecli: start: %w", err)
	}

	res, parseErr := parseStream(stdout)
	if errors.Is(parseErr, llm.ErrToolsExposed) {
		cancel() // kill before the model can act
	}
	waitErr := cmd.Wait()
	if parseErr != nil {
		return nil, parseErr
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("claudecli: %w", ctx.Err())
	}
	if res.final == nil {
		if waitErr == nil {
			waitErr = errors.New("exit 0")
		}
		return nil, fmt.Errorf("claudecli: no result event (%w, stderr: %s)", waitErr, firstLine(stderr.String()))
	}
	f := res.final
	if f.IsError {
		return nil, fmt.Errorf("claudecli: %s (%s)", truncate(f.Result, 200), f.Subtype)
	}
	out := f.StructuredOutput
	if len(out) == 0 || string(out) == "null" {
		return nil, llm.ErrNoStructuredOutput
	}
	return &llm.Response{
		Output:  out,
		Model:   res.model,
		CostUSD: f.TotalCostUSD,
		Usage: llm.Usage{
			InputTokens:       f.Usage.InputTokens,
			OutputTokens:      f.Usage.OutputTokens,
			CacheReadTokens:   f.Usage.CacheReadInputTokens,
			CacheCreateTokens: f.Usage.CacheCreationInputTokens,
		},
		Duration: time.Since(start),
	}, nil
}

type event struct {
	Type       string   `json:"type"`
	Subtype    string   `json:"subtype"`
	Tools      []string `json:"tools"`
	MCPServers []struct {
		Name string `json:"name"`
	} `json:"mcp_servers"`
	Model string `json:"model"`
}

type resultEvent struct {
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Usage            struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type streamResult struct {
	sawInit bool
	model   string
	final   *resultEvent
}

// parseStream reads newline-delimited JSON events. The init event must come
// before anything else and must expose no tool beyond StructuredOutput.
func parseStream(r interface{ Read([]byte) (int, error) }) (*streamResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	res := &streamResult{}
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // tolerate non-JSON noise
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			if err := checkInit(ev); err != nil {
				return res, err
			}
			res.sawInit = true
			res.model = ev.Model
		case ev.Type == "result":
			if !res.sawInit {
				return res, fmt.Errorf("%w: result before init event", llm.ErrToolsExposed)
			}
			var re resultEvent
			if err := json.Unmarshal(line, &re); err != nil {
				return res, fmt.Errorf("claudecli: decode result: %w", err)
			}
			res.final = &re
		case !res.sawInit && ev.Type != "system":
			// Anything model-related before init means we could not verify
			// the tool list: refuse.
			return res, fmt.Errorf("%w: %q event before init", llm.ErrToolsExposed, ev.Type)
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("claudecli: read stream: %w", err)
	}
	return res, nil
}

func checkInit(ev event) error {
	for _, t := range ev.Tools {
		if !slices.Contains(allowedTools, t) {
			return fmt.Errorf("%w: tool %q", llm.ErrToolsExposed, t)
		}
	}
	if len(ev.MCPServers) > 0 {
		return fmt.Errorf("%w: MCP server %q", llm.ErrToolsExposed, ev.MCPServers[0].Name)
	}
	return nil
}

// Version runs `claude --version`.
func (p *Provider) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.cfg.Binary, "--version")
	cmd.Env = p.env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claudecli: %s --version: %w", p.cfg.Binary, err)
	}
	return strings.TrimSpace(string(out)), nil
}

type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(b) > room {
			w.buf.Write(b[:room])
		} else {
			w.buf.Write(b)
		}
	}
	return len(b), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return truncate(s, 200)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
