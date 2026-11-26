// Package config loads mailpilot's configuration: a non-secret YAML file plus
// secrets read from the environment or from mounted files (*_FILE variables).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Security selects how a mail connection is protected. There is deliberately
// no plaintext mode: credentials never travel unencrypted.
type Security string

const (
	SecurityTLS      Security = "tls"      // implicit TLS (IMAPS 993, SMTPS 465)
	SecurityStartTLS Security = "starttls" // STARTTLS upgrade (SMTP 587)
)

// Config is the full, validated configuration.
type Config struct {
	Timezone    string         `yaml:"timezone"`
	LogLevel    string         `yaml:"log_level"`
	DataDir     string         `yaml:"data_dir"`
	SelfAliases []string       `yaml:"self_aliases"`
	PromptsDir  string         `yaml:"prompts_dir"`
	RulesPath   string         `yaml:"rules_path"`
	ProfilePath string         `yaml:"profile_path"`
	IMAP        IMAPConfig     `yaml:"imap"`
	SMTP        SMTPConfig     `yaml:"smtp"`
	Telegram    TelegramConfig `yaml:"telegram"`
	LLM         LLMConfig      `yaml:"llm"`
	Todoist     TodoistConfig  `yaml:"todoist"`
	Digest      DigestConfig   `yaml:"digest"`
	Retention   RetentionCfg   `yaml:"retention"`
	Web         WebConfig      `yaml:"web"`

	// Location is Timezone resolved at load time.
	Location *time.Location `yaml:"-"`
}

// IMAPConfig describes the monitored mailbox.
type IMAPConfig struct {
	Host               string        `yaml:"host"`
	Port               int           `yaml:"port"`
	Security           Security      `yaml:"security"`
	Username           string        `yaml:"username"`
	Mailbox            string        `yaml:"mailbox"`
	IdleRefresh        time.Duration `yaml:"idle_refresh"`
	PollInterval       time.Duration `yaml:"poll_interval"`
	BootstrapDays      int           `yaml:"bootstrap_days"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify"`
	Timeout            time.Duration `yaml:"timeout"`
	// Folders for drafts and sent copies. Empty: detected with SPECIAL-USE;
	// without SPECIAL-USE they must be set, or drafts are refused.
	DraftsFolder string `yaml:"drafts_folder"`
	SentFolder   string `yaml:"sent_folder"`
	// Folders for the web "Archiver" and "Supprimer" buttons, detected the
	// same way. Deleting only ever moves to the trash.
	ArchiveFolder string `yaml:"archive_folder"`
	TrashFolder   string `yaml:"trash_folder"`
	// SaveSent appends a copy of each sent reply to the sent folder (turn
	// off if the server already does it).
	SaveSent bool `yaml:"save_sent"`

	Password Secret `yaml:"-"`
}

// SMTPConfig describes the submission server. Sending is off by default.
type SMTPConfig struct {
	Host               string        `yaml:"host"`
	Port               int           `yaml:"port"`
	Security           Security      `yaml:"security"`
	Username           string        `yaml:"username"`
	SendEnabled        bool          `yaml:"send_enabled"`
	FromName           string        `yaml:"from_name"`
	MaxSendsPerHour    int           `yaml:"max_sends_per_hour"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify"`
	Timeout            time.Duration `yaml:"timeout"`

	Password Secret `yaml:"-"`
}

// TelegramConfig configures the bot. Only ChatID is ever answered.
type TelegramConfig struct {
	ChatID  int64  `yaml:"chat_id"`
	APIBase string `yaml:"api_base"`

	Token Secret `yaml:"-"`
}

// LLMConfig selects the provider and the model used for each task.
type LLMConfig struct {
	Provider  string             `yaml:"provider"`
	ClaudeCLI ClaudeCLIConfig    `yaml:"claude_cli"`
	Tasks     map[string]LLMTask `yaml:"tasks"`
}

// ClaudeCLIConfig configures the `claude -p` provider.
type ClaudeCLIConfig struct {
	Binary         string        `yaml:"binary"`
	Timeout        time.Duration `yaml:"timeout"`
	MaxConcurrency int           `yaml:"max_concurrency"`
}

// LLMTask is the model choice for one task (triage, draft, ask).
type LLMTask struct {
	Model        string  `yaml:"model"`
	Effort       string  `yaml:"effort"`
	MaxBudgetUSD float64 `yaml:"max_budget_usd"`
}

// TodoistConfig configures reminders.
type TodoistConfig struct {
	Enabled     bool     `yaml:"enabled"`
	APIBase     string   `yaml:"api_base"`
	ModeDated   string   `yaml:"mode_dated"`
	ModeUndated string   `yaml:"mode_undated"`
	ProjectID   string   `yaml:"project_id"`
	Labels      []string `yaml:"labels"`

	Token Secret `yaml:"-"`
}

// DigestConfig lists the local times ("HH:MM") at which digests are sent.
type DigestConfig struct {
	Times []string `yaml:"times"`
}

// RetentionCfg controls how long message bodies are kept.
type RetentionCfg struct {
	BodyDays int `yaml:"body_days"`
}

// WebConfig configures the web interface (off by default). It listens on
// the loopback interface unless allow_remote is set.
type WebConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Listen      string        `yaml:"listen"`
	AllowRemote bool          `yaml:"allow_remote"`
	PublicURL   string        `yaml:"public_url"`
	SessionTTL  time.Duration `yaml:"session_ttl"`
	// MarkReadOnOpen sets \Seen when Louis opens a mail in the interface
	// (never when the list or a notification shows it).
	MarkReadOnOpen bool `yaml:"mark_read_on_open"`

	// PasswordHash is an argon2id PHC string (mailpilot web-password).
	PasswordHash Secret `yaml:"-"`
}

// LLM task names.
const (
	TaskTriage = "triage"
	TaskDraft  = "draft"
	TaskAsk    = "ask"
)

// Default returns the configuration used when a key is absent from the file.
func Default() Config {
	return Config{
		Timezone: "Europe/Paris",
		LogLevel: "info",
		DataDir:  "/data",
		// Files here replace the embedded prompts (missing dir = defaults).
		PromptsDir:  "/config/prompts",
		RulesPath:   "/config/rules.yaml",
		ProfilePath: "/config/profile.md",
		IMAP: IMAPConfig{
			Host:          "pro3.mail.ovh.net",
			Port:          993,
			Security:      SecurityTLS,
			Mailbox:       "INBOX",
			IdleRefresh:   25 * time.Minute,
			PollInterval:  90 * time.Second,
			BootstrapDays: 7,
			Timeout:       30 * time.Second,
			SaveSent:      true,
		},
		SMTP: SMTPConfig{
			Host:            "pro3.mail.ovh.net",
			Port:            587,
			Security:        SecurityStartTLS,
			Timeout:         30 * time.Second,
			MaxSendsPerHour: 10,
		},
		Telegram: TelegramConfig{APIBase: "https://api.telegram.org"},
		LLM: LLMConfig{
			Provider: "claude-cli",
			ClaudeCLI: ClaudeCLIConfig{
				Binary:         "claude",
				Timeout:        90 * time.Second,
				MaxConcurrency: 1,
			},
			Tasks: map[string]LLMTask{
				TaskTriage: {Model: "claude-opus-5-5", Effort: "low", MaxBudgetUSD: 0.5},
				TaskDraft:  {Model: "claude-opus-5-5", Effort: "medium", MaxBudgetUSD: 1},
				TaskAsk:    {Model: "claude-opus-5-5", Effort: "medium", MaxBudgetUSD: 1},
			},
		},
		Todoist: TodoistConfig{
			APIBase:     "https://api.todoist.com/api/v1",
			ModeDated:   "auto",
			ModeUndated: "confirm",
			Labels:      []string{"mailpilot"},
		},
		Digest:    DigestConfig{Times: []string{"08:30", "18:00"}},
		Retention: RetentionCfg{BodyDays: 30},
		Web:       WebConfig{Listen: "127.0.0.1:8484", SessionTTL: 30 * 24 * time.Hour, MarkReadOnOpen: true},
	}
}

// Env is the environment lookup used by Load; tests substitute their own.
type Env func(key string) (string, bool)

// Load reads the YAML file at path (an empty path means defaults only), then
// secrets from env, then validates the result.
func Load(path string, env Env) (*Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := decodeStrict(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := loadSecrets(&cfg, env); err != nil {
		return nil, err
	}
	if cfg.SMTP.Username == "" {
		cfg.SMTP.Username = cfg.IMAP.Username
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func decodeStrict(data []byte, out *Config) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data), yaml.DisallowUnknownField())
	return dec.Decode(out)
}

// Environment variable names for secrets. Each also accepts a NAME_FILE
// variant pointing to a mounted file.
const (
	EnvIMAPPassword  = "MAILPILOT_IMAP_PASSWORD"
	EnvSMTPPassword  = "MAILPILOT_SMTP_PASSWORD"
	EnvTelegramToken = "MAILPILOT_TELEGRAM_TOKEN"
	EnvTodoistToken  = "MAILPILOT_TODOIST_TOKEN"
	EnvWebPassword   = "MAILPILOT_WEB_PASSWORD_HASH"
)

func loadSecrets(cfg *Config, env Env) error {
	var err error
	if cfg.IMAP.Password, err = secretFromEnv(env, EnvIMAPPassword); err != nil {
		return err
	}
	if cfg.SMTP.Password, err = secretFromEnv(env, EnvSMTPPassword); err != nil {
		return err
	}
	if cfg.SMTP.Password.Empty() {
		cfg.SMTP.Password = cfg.IMAP.Password
	}
	if cfg.Telegram.Token, err = secretFromEnv(env, EnvTelegramToken); err != nil {
		return err
	}
	if cfg.Todoist.Token, err = secretFromEnv(env, EnvTodoistToken); err != nil {
		return err
	}
	if cfg.Web.PasswordHash, err = secretFromEnv(env, EnvWebPassword); err != nil {
		return err
	}
	return nil
}

func secretFromEnv(env Env, name string) (Secret, error) {
	if v, ok := env(name); ok && v != "" {
		return NewSecret(v), nil
	}
	if path, ok := env(name + "_FILE"); ok && path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // operator-supplied secret file
		if err != nil {
			return Secret{}, fmt.Errorf("read %s_FILE: %w", name, err)
		}
		return NewSecret(strings.TrimRight(string(data), "\r\n")), nil
	}
	return Secret{}, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		add("timezone %q: %v", c.Timezone, err)
	}
	c.Location = loc

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("log_level must be debug, info, warn or error, got %q", c.LogLevel)
	}
	if c.DataDir == "" {
		add("data_dir is required")
	}

	if c.IMAP.Host == "" || c.IMAP.Port <= 0 {
		add("imap.host and imap.port are required")
	}
	if c.IMAP.Username == "" {
		add("imap.username is required")
	}
	if c.IMAP.Security != SecurityTLS && c.IMAP.Security != SecurityStartTLS {
		add("imap.security must be tls or starttls, got %q", c.IMAP.Security)
	}
	if c.IMAP.IdleRefresh <= 0 || c.IMAP.IdleRefresh >= 29*time.Minute {
		add("imap.idle_refresh must be between 0 and 29m (RFC 2177), got %s", c.IMAP.IdleRefresh)
	}
	if c.IMAP.PollInterval < 10*time.Second {
		add("imap.poll_interval must be at least 10s")
	}
	if c.IMAP.BootstrapDays < 0 {
		add("imap.bootstrap_days must be >= 0")
	}

	if c.SMTP.Host == "" || c.SMTP.Port <= 0 {
		add("smtp.host and smtp.port are required")
	}
	if c.SMTP.MaxSendsPerHour < 1 || c.SMTP.MaxSendsPerHour > 100 {
		add("smtp.max_sends_per_hour must be between 1 and 100")
	}
	if c.SMTP.Security != SecurityTLS && c.SMTP.Security != SecurityStartTLS {
		add("smtp.security must be tls or starttls, got %q", c.SMTP.Security)
	}

	if c.LLM.Provider != "claude-cli" {
		add("llm.provider %q is not supported yet (only claude-cli)", c.LLM.Provider)
	}
	if c.LLM.ClaudeCLI.MaxConcurrency < 1 {
		add("llm.claude_cli.max_concurrency must be >= 1")
	}
	if c.LLM.ClaudeCLI.Timeout <= 0 {
		add("llm.claude_cli.timeout must be > 0")
	}
	for _, task := range []string{TaskTriage, TaskDraft, TaskAsk} {
		t, ok := c.LLM.Tasks[task]
		if !ok || t.Model == "" {
			add("llm.tasks.%s.model is required", task)
			continue
		}
		switch t.Effort {
		case "", "low", "medium", "high", "xhigh", "max":
		default:
			add("llm.tasks.%s.effort %q is invalid", task, t.Effort)
		}
	}

	for _, m := range []struct{ key, v string }{{"mode_dated", c.Todoist.ModeDated}, {"mode_undated", c.Todoist.ModeUndated}} {
		if m.v != "auto" && m.v != "confirm" && m.v != "off" {
			add("todoist.%s must be auto, confirm or off, got %q", m.key, m.v)
		}
	}
	for _, t := range c.Digest.Times {
		if _, err := time.Parse("15:04", t); err != nil {
			add("digest.times: %q is not HH:MM", t)
		}
	}
	if c.Retention.BodyDays < 1 {
		add("retention.body_days must be >= 1")
	}
	c.validateWeb(add)
	return errors.Join(errs...)
}

// Address returns host:port.
func (c IMAPConfig) Address() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

// Address returns host:port.
func (c SMTPConfig) Address() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

func (c *Config) validateWeb(add func(string, ...any)) {
	w := c.Web
	if !w.Enabled {
		return
	}
	host, port, err := net.SplitHostPort(w.Listen)
	if err != nil || port == "" {
		add("web.listen must be host:port, got %q", w.Listen)
		return
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !loopback && !w.AllowRemote {
		add("web.listen %q is not a loopback address: set web.allow_remote: true to expose the interface (behind HTTPS)", w.Listen)
	}
	if w.PasswordHash.Empty() {
		add("web.enabled is true but %s is empty (generate it with: mailpilot web-password)", EnvWebPassword)
	} else if !strings.HasPrefix(w.PasswordHash.Reveal(), "$argon2id$") {
		add("%s must be an argon2id hash (mailpilot web-password)", EnvWebPassword)
	}
	if w.PublicURL != "" {
		u, err := url.Parse(w.PublicURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add("web.public_url must be an absolute http(s) URL, got %q", w.PublicURL)
		} else if u.Scheme != "https" && w.AllowRemote {
			add("web.public_url must use https when web.allow_remote is true")
		}
	}
	if w.SessionTTL < time.Hour || w.SessionTTL > 90*24*time.Hour {
		add("web.session_ttl must be between 1h and 2160h")
	}
}
