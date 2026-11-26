// Command mailpilot watches a professional mailbox, summarises each email
// with an LLM and notifies on Telegram.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	_ "time/tzdata" // Europe/Paris must resolve in a distroless image

	"github.com/Vodki/mailpilot/internal/analysis"
	"github.com/Vodki/mailpilot/internal/app"
	"github.com/Vodki/mailpilot/internal/check"
	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/imapwatch"
	"github.com/Vodki/mailpilot/internal/llm/claudecli"
	"github.com/Vodki/mailpilot/internal/logx"
	"github.com/Vodki/mailpilot/internal/mailbox"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
	"github.com/Vodki/mailpilot/internal/todoist"
	"github.com/Vodki/mailpilot/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage : mailpilot <commande> [options]

Commandes :
  run       démarre le service
  check     teste IMAP, SMTP, Telegram, le LLM et Todoist sans rien modifier
  migrate   applique les migrations de la base
  web-password  calcule le hash du mot de passe de l'interface web (lu sur l'entrée standard)
  version   affiche la version

Options communes :
  -config chemin   fichier de configuration (défaut : $MAILPILOT_CONFIG ou /config/config.yaml)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "version", "-version", "--version":
		_, _ = fmt.Fprintln(stdout, "mailpilot", version)
		return 0
	case "check":
		return cmdCheck(ctx, rest, stdout, stderr)
	case "migrate":
		return cmdMigrate(ctx, rest, stdout, stderr)
	case "run":
		return cmdRun(ctx, rest, stderr)
	case "web-password":
		return cmdWebPassword(os.Stdin, stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "commande inconnue %q\n\n%s", cmd, usage)
		return 2
	}
}

func defaultConfigPath() string {
	if p := os.Getenv("MAILPILOT_CONFIG"); p != "" {
		return p
	}
	return "/config/config.yaml"
}

func loadConfig(fs *flag.FlagSet, args []string, stderr io.Writer) (*config.Config, error) {
	path := fs.String("config", defaultConfigPath(), "fichier de configuration")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg, err := config.Load(*path, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "configuration invalide :\n%v\n", err)
		return nil, err
	}
	return cfg, nil
}

func cmdCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	only := fs.String("only", "", "vérifications à lancer, séparées par des virgules ("+strings.Join(check.Names, ",")+")")
	skip := fs.String("skip", "", "vérifications à ignorer, séparées par des virgules")
	cfg, err := loadConfig(fs, args, stderr)
	if err != nil {
		return 2
	}
	selected, err := selectChecks(*only, *skip)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if !check.Print(stdout, check.Run(ctx, cfg, selected)) {
		return 1
	}
	return 0
}

func selectChecks(only, skip string) ([]string, error) {
	split := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	known := map[string]bool{}
	for _, n := range check.Names {
		known[n] = true
	}
	sel := split(only)
	if len(sel) == 0 {
		sel = append(sel, check.Names...)
	}
	skipped := map[string]bool{}
	for _, n := range append(split(skip), split(only)...) {
		if !known[n] {
			return nil, fmt.Errorf("vérification inconnue %q (connues : %s)", n, strings.Join(check.Names, ", "))
		}
	}
	for _, n := range split(skip) {
		skipped[n] = true
	}
	var out []string
	for _, n := range sel {
		if !skipped[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

func openStore(ctx context.Context, cfg *config.Config) (*store.Store, int, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil { //nolint:gosec // operator-supplied data dir
		return nil, 0, err
	}
	s, err := store.Open(ctx, filepath.Join(cfg.DataDir, "mailpilot.db"))
	if err != nil {
		return nil, 0, err
	}
	n, err := s.Migrate(ctx)
	if err != nil {
		_ = s.Close()
		return nil, 0, err
	}
	return s, n, nil
}

func cmdMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg, err := loadConfig(fs, args, stderr)
	if err != nil {
		return 2
	}
	s, n, err := openStore(ctx, cfg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "migration :", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	_, _ = fmt.Fprintf(stdout, "%d migration(s) appliquée(s), schéma v%d\n", n, store.LatestVersion())
	return 0
}

func cmdRun(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg, err := loadConfig(fs, args, stderr)
	if err != nil {
		return 2
	}
	log := logx.New(stderr, cfg.LogLevel)
	slog.SetDefault(log)
	if cfg.Telegram.Token.Empty() || cfg.Telegram.ChatID == 0 {
		log.Error("telegram.chat_id and MAILPILOT_TELEGRAM_TOKEN are required to run")
		return 2
	}

	s, n, err := openStore(ctx, cfg)
	if err != nil {
		log.Error("store", "err", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	if err := s.EnsureAccount(ctx, 1, cfg.IMAP.Username); err != nil {
		log.Error("store account", "err", err)
		return 1
	}
	log.Info("starting", "version", version, "schema", store.LatestVersion(), "migrations_applied", n,
		"imap", cfg.IMAP.Address(), "send_enabled", cfg.SMTP.SendEnabled)

	prompts, err := analysis.LoadPrompts(cfg.PromptsDir)
	if err != nil {
		log.Error("prompts", "err", err)
		return 1
	}
	tg := telegram.New(cfg.Telegram.APIBase, cfg.Telegram.Token, nil)
	mailOps := mailbox.NewOps(cfg.IMAP)
	deps := app.Deps{
		Config: cfg, Store: s, Provider: claudecli.New(cfg.LLM.ClaudeCLI, cfg.LLM.Tasks),
		Prompts: prompts, TG: tg, Log: log, Version: version,
		Ops: mailOps,
	}
	if cfg.Todoist.Enabled {
		if cfg.Todoist.Token.Empty() {
			log.Error("todoist.enabled is true but MAILPILOT_TODOIST_TOKEN is empty")
			return 2
		}
		deps.Todoist = todoist.New(cfg.Todoist.APIBase, cfg.Todoist.Token, nil)
	}
	a, err := app.New(deps)
	if err != nil {
		log.Error("init", "err", err)
		return 1
	}
	w := imapwatch.New(cfg.IMAP, a.Sink(), log)
	a.SetWatcher(w)
	if err := a.Start(ctx); err != nil {
		log.Error("start", "err", err)
		return 1
	}
	if err := tg.SetMyCommands(ctx, a.Commands()); err != nil {
		log.Warn("telegram setMyCommands", "err", err)
	}
	a.Announce(ctx)

	// SIGHUP reloads rules.yaml, profile.md and prompts.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				a.ReloadAndReport(ctx)
			}
		}
	}()

	done := make(chan struct{}, 3)
	workers := 2
	go func() { a.Bot(tg).Run(ctx); done <- struct{}{} }()
	go func() { _ = w.Run(ctx); done <- struct{}{} }()
	if cfg.Web.Enabled {
		ws, err := web.New(web.Deps{Config: cfg, Store: s, Mailbox: webMailbox{mailOps}, Composer: webComposer{a}, Assistant: webAssistant{a}, Log: log, Version: version})
		if err != nil {
			log.Error("web", "err", err)
			return 1
		}
		workers++
		go func() {
			if err := ws.Run(ctx); err != nil {
				log.Error("web interface stopped", "err", err)
			}
			done <- struct{}{}
		}()
	}
	<-ctx.Done()
	for range workers {
		<-done
	}
	a.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("shutdown", "err", err)
	}
	log.Info("stopped")
	return 0
}

// cmdWebPassword reads a password on stdin and prints its argon2id hash,
// to put in MAILPILOT_WEB_PASSWORD_HASH.
func cmdWebPassword(stdin io.Reader, stdout, stderr io.Writer) int {
	_, _ = fmt.Fprint(stderr, "Mot de passe de l'interface web (12 caractères minimum) : ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		_, _ = fmt.Fprintln(stderr, "\naucun mot de passe lu")
		return 2
	}
	hash, err := web.HashPassword(strings.TrimRight(line, "\r\n"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "\n"+err.Error())
		return 2
	}
	_, _ = fmt.Fprintln(stderr)
	_, _ = fmt.Fprintln(stdout, hash)
	return 0
}
