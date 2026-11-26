// Package check implements `mailpilot check`: it tests every external
// dependency without changing anything (no mail sent, no flag set, no task
// created) and prints a report in French.
package check

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/llm"
	"github.com/Vodki/mailpilot/internal/llm/claudecli"
	"github.com/Vodki/mailpilot/internal/mailbox"
	"github.com/Vodki/mailpilot/internal/reply"
	"github.com/Vodki/mailpilot/internal/store"
	"github.com/Vodki/mailpilot/internal/telegram"
	"github.com/Vodki/mailpilot/internal/todoist"
)

// Status of one check.
type Status int

const (
	OK Status = iota
	Skipped
	Failed
)

// Result of one check. Lines are shown indented under the title.
type Result struct {
	Name   string
	Status Status
	Lines  []string
}

// Names of the available checks, in execution order.
var Names = []string{"store", "imap", "smtp", "telegram", "llm", "todoist"}

// Run executes the selected checks (all when only is empty).
func Run(ctx context.Context, cfg *config.Config, only []string) []Result {
	want := func(n string) bool {
		if len(only) == 0 {
			return true
		}
		for _, o := range only {
			if o == n {
				return true
			}
		}
		return false
	}
	runners := map[string]func(context.Context, *config.Config) Result{
		"store": checkStore, "imap": checkIMAP, "smtp": checkSMTP,
		"telegram": checkTelegram, "llm": checkLLM, "todoist": checkTodoist,
	}
	var out []Result
	for _, n := range Names {
		if !want(n) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		out = append(out, runners[n](cctx, cfg))
		cancel()
	}
	return out
}

// Print writes the report and returns true if no check failed.
func Print(w io.Writer, results []Result) bool {
	ok := true
	for _, r := range results {
		mark := "OK     "
		switch r.Status {
		case Skipped:
			mark = "IGNORÉ "
		case Failed:
			mark = "ÉCHEC  "
			ok = false
		}
		_, _ = fmt.Fprintf(w, "[%s] %s\n", mark, r.Name)
		for _, l := range r.Lines {
			_, _ = fmt.Fprintf(w, "          %s\n", l)
		}
	}
	if ok {
		_, _ = fmt.Fprintln(w, "\nTout est prêt.")
	} else {
		_, _ = fmt.Fprintln(w, "\nAu moins une vérification a échoué.")
	}
	return ok
}

func failed(name string, err error, lines ...string) Result {
	return Result{Name: name, Status: Failed, Lines: append(lines, "erreur : "+err.Error())}
}

func checkStore(ctx context.Context, cfg *config.Config) Result {
	const name = "Base SQLite"
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil { //nolint:gosec // operator-supplied data dir
		return failed(name, err)
	}
	path := filepath.Join(cfg.DataDir, "mailpilot.db")
	s, err := store.Open(ctx, path)
	if err != nil {
		return failed(name, err)
	}
	defer func() { _ = s.Close() }()
	v, err := s.Version(ctx)
	if err != nil {
		return failed(name, err)
	}
	lines := []string{"fichier : " + path, fmt.Sprintf("schéma : v%d (binaire : v%d)", v, store.LatestVersion())}
	if v > store.LatestVersion() {
		return failed(name, fmt.Errorf("base plus récente que le binaire"), lines...)
	}
	if v < store.LatestVersion() {
		lines = append(lines, "migrations en attente : appliquées au prochain démarrage ou par `mailpilot migrate`")
	}
	return Result{Name: name, Status: OK, Lines: lines}
}

func checkIMAP(ctx context.Context, cfg *config.Config) Result {
	name := "IMAP " + cfg.IMAP.Address()
	var lines []string
	if cfg.IMAP.InsecureSkipVerify {
		lines = append(lines, "ATTENTION : vérification du certificat désactivée (serveur de test uniquement)")
	}
	if cfg.IMAP.Password.Empty() {
		return failed(name, fmt.Errorf("mot de passe absent (%s)", config.EnvIMAPPassword), lines...)
	}
	res, err := mailbox.Probe(ctx, cfg.IMAP)
	if res != nil && len(res.CapsBeforeLogin) > 0 {
		lines = append(lines, "CAPABILITY avant login : "+strings.Join(res.CapsBeforeLogin, " "))
	}
	if err != nil {
		return failed(name, err, lines...)
	}
	lines = append(lines, "CAPABILITY après login : "+strings.Join(res.CapsAfterLogin, " "))
	if res.HasIdle {
		lines = append(lines, "IDLE : oui (surveillance en temps réel)")
	} else {
		lines = append(lines, fmt.Sprintf("IDLE : non, repli sur un relevé toutes les %s", cfg.IMAP.PollInterval))
	}
	if res.HasSpecialUse {
		lines = append(lines, "SPECIAL-USE : oui")
		for _, attr := range []string{`\Drafts`, `\Sent`, `\Trash`, `\Junk`} {
			if mb, ok := res.SpecialUse[attr]; ok {
				lines = append(lines, fmt.Sprintf("  %s → %q", attr, mb))
			}
		}
	} else {
		lines = append(lines, "SPECIAL-USE : non, dossiers Brouillons et Envoyés à configurer (phase 3)")
	}
	lines = append(lines, fmt.Sprintf("%s : %d messages, UIDVALIDITY %d, UIDNEXT %d (ouvert en lecture seule)",
		res.Mailbox, res.NumMessages, res.UIDValidity, res.UIDNext))
	return Result{Name: name, Status: OK, Lines: lines}
}

func checkSMTP(ctx context.Context, cfg *config.Config) Result {
	name := "SMTP " + cfg.SMTP.Address()
	var lines []string
	if cfg.SMTP.InsecureSkipVerify {
		lines = append(lines, "ATTENTION : vérification du certificat désactivée (serveur de test uniquement)")
	}
	if cfg.SMTP.Password.Empty() {
		return failed(name, fmt.Errorf("mot de passe absent (%s ou %s)", config.EnvSMTPPassword, config.EnvIMAPPassword), lines...)
	}
	res, err := reply.ProbeSMTP(ctx, cfg.SMTP)
	if err != nil {
		return failed(name, err, lines...)
	}
	lines = append(lines, "authentification réussie (AUTH "+res.AuthMechanisms+"), aucun message envoyé")
	if cfg.SMTP.SendEnabled {
		lines = append(lines, "envoi : ACTIVÉ (send_enabled: true)")
	} else {
		lines = append(lines, "envoi : désactivé (send_enabled: false)")
	}
	return Result{Name: name, Status: OK, Lines: lines}
}

func checkTelegram(ctx context.Context, cfg *config.Config) Result {
	const name = "Telegram"
	if cfg.Telegram.Token.Empty() {
		return Result{Name: name, Status: Skipped, Lines: []string{"jeton absent (" + config.EnvTelegramToken + ")"}}
	}
	u, err := telegram.New(cfg.Telegram.APIBase, cfg.Telegram.Token, nil).GetMe(ctx)
	if err != nil {
		return failed(name, err)
	}
	lines := []string{fmt.Sprintf("bot @%s (id %d)", u.Username, u.ID)}
	if cfg.Telegram.ChatID == 0 {
		return Result{Name: name, Status: Failed, Lines: append(lines, "erreur : telegram.chat_id n'est pas configuré")}
	}
	lines = append(lines, fmt.Sprintf("seul chat autorisé : %d", cfg.Telegram.ChatID))
	return Result{Name: name, Status: OK, Lines: lines}
}

// probeSchema is the tiny schema used to prove auth and structured output.
const probeSchema = `{"type":"object","properties":{"status":{"type":"string","enum":["ok"]}},"required":["status"],"additionalProperties":false}`

func checkLLM(ctx context.Context, cfg *config.Config) Result {
	name := "LLM (" + cfg.LLM.Provider + ")"
	p := claudecli.New(cfg.LLM.ClaudeCLI, cfg.LLM.Tasks)
	v, err := p.Version(ctx)
	if err != nil {
		return failed(name, err)
	}
	lines := []string{"version : " + v, "modèle de tri : " + cfg.LLM.Tasks[config.TaskTriage].Model}
	res, err := p.Complete(ctx, llm.Request{
		Task:   config.TaskTriage,
		System: "Tu es un test de connexion. Réponds uniquement avec le JSON demandé.",
		Prompt: "Renvoie status = ok.",
		Schema: []byte(probeSchema),
	})
	if err != nil {
		return failed(name, err, lines...)
	}
	lines = append(lines,
		"aucun outil ni serveur MCP exposé au modèle (vérifié)",
		fmt.Sprintf("réponse structurée : %s en %s, %d+%d tokens, coût indicatif %.4f $",
			res.Output, res.Duration.Round(time.Millisecond), res.Usage.InputTokens, res.Usage.OutputTokens, res.CostUSD))
	return Result{Name: name, Status: OK, Lines: lines}
}

func checkTodoist(ctx context.Context, cfg *config.Config) Result {
	const name = "Todoist"
	if cfg.Todoist.Token.Empty() {
		return Result{Name: name, Status: Skipped, Lines: []string{"jeton absent (" + config.EnvTodoistToken + ")"}}
	}
	u, err := todoist.New(cfg.Todoist.APIBase, cfg.Todoist.Token, nil).CurrentUser(ctx)
	if err != nil {
		return failed(name, err)
	}
	return Result{Name: name, Status: OK, Lines: []string{"connecté (utilisateur " + u.ID + ")"}}
}
