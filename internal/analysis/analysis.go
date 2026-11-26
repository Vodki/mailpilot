// Package analysis builds the triage prompt, calls the LLM provider and
// validates its JSON output. Email content is untrusted: it is wrapped in
// <email untrusted="true"> tags, any tag look-alike inside it is neutralised,
// and the output is checked in Go (schema, lengths, dates, quotes) before any
// other package may act on it.
package analysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/llm"
)

//go:embed prompts/*
var embedded embed.FS

// Prompts are the system prompts and schemas. Defaults are embedded; files
// with the same names in an override directory replace them.
type Prompts struct {
	TriageSystem string
	DraftSystem  string
	AskSystem    string
}

// promptFiles maps override file names to fields.
var promptFiles = []string{"triage_system.md", "draft_system.md", "ask_system.md"}

// LoadPrompts returns the embedded prompts, replaced by any file of the same
// name found in dir (dir may be empty or missing).
func LoadPrompts(dir string) (Prompts, error) {
	vals := make([]string, len(promptFiles))
	for i, name := range promptFiles {
		b, err := embedded.ReadFile("prompts/" + name)
		if err != nil {
			return Prompts{}, err
		}
		vals[i] = string(b)
		if dir == "" {
			continue
		}
		o, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // operator-supplied prompt dir
		switch {
		case err == nil:
			if strings.TrimSpace(string(o)) == "" {
				return Prompts{}, fmt.Errorf("prompt override %s is empty", name)
			}
			vals[i] = string(o)
		case errors.Is(err, os.ErrNotExist):
		default:
			return Prompts{}, fmt.Errorf("read prompt %s: %w", name, err)
		}
	}
	return Prompts{TriageSystem: vals[0], DraftSystem: vals[1], AskSystem: vals[2]}, nil
}

// MustSchema returns an embedded schema.
func MustSchema(name string) json.RawMessage {
	b, err := embedded.ReadFile("prompts/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// Validator validates a JSON document against a compiled schema.
type Validator struct{ schema *jsonschema.Schema }

// NewValidator compiles a JSON Schema.
func NewValidator(name string, schema json.RawMessage) (*Validator, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	url := "mem://" + name
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, err
	}
	return &Validator{schema: s}, nil
}

// Validate checks raw JSON.
func (v *Validator) Validate(raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("JSON invalide : %w", err)
	}
	return v.schema.Validate(inst)
}

// Email is the input of one analysis. Header-derived fields are trusted;
// Text is not.
type Email struct {
	From        string
	To          []string
	Cc          []string
	Subject     string
	Date        time.Time
	Direct      bool // Louis is in To
	ListID      string
	Attachments []string // "name (mime, size)"
	Text        string   // cleaned body
}

// Input is everything the triage prompt needs.
type Input struct {
	Email   Email
	Thread  []Email // previous messages of the thread, oldest first
	Profile string  // profile.md, trusted (written by Louis)
	Extra   string  // extra_instructions from rules.yaml, trusted
	Now     time.Time
}

// Action is a task extracted from a mail.
type Action struct {
	Text        string `json:"text"`
	Owner       string `json:"owner"`
	DueDate     string `json:"due_date"`
	DueTime     string `json:"due_time"`
	SourceQuote string `json:"source_quote"`

	// Set by Go validation, never by the model.
	QuoteVerified bool `json:"quote_verified"`
	DateDropped   bool `json:"date_dropped,omitempty"`
}

// Triage is the validated triage output.
type Triage struct {
	Category               string   `json:"category"`
	Priority               string   `json:"priority"`
	Summary                string   `json:"summary"`
	NeedsReply             bool     `json:"needs_reply"`
	Actions                []Action `json:"actions"`
	ContainsAIInstructions bool     `json:"contains_ai_instructions"`
	Reason                 string   `json:"reason"`

	// InjectionDetected is the Go heuristic, ORed into AIInstructions().
	InjectionDetected bool `json:"injection_detected"`
}

// AIInstructions reports whether the mail tries to talk to an AI, by the
// model's own judgement or by the Go detector.
func (t *Triage) AIInstructions() bool { return t.ContainsAIInstructions || t.InjectionDetected }

// Attempt is one provider call, for the audit trail and cost tracking.
type Attempt struct {
	N        int
	Status   string // ok | invalid | error
	Error    string
	Response *llm.Response // nil on provider error
}

// Outcome is the result of Analyze.
type Outcome struct {
	Triage     *Triage // nil if every attempt failed
	Attempts   []Attempt
	PromptHash string
	Model      string
}

// Analyzer runs triage.
type Analyzer struct {
	provider  llm.Provider
	prompts   Prompts
	validator *Validator
	schema    json.RawMessage
	loc       *time.Location
}

// New builds an analyzer.
func New(p llm.Provider, prompts Prompts, loc *time.Location) (*Analyzer, error) {
	schema := MustSchema("triage_schema.json")
	v, err := NewValidator("triage", schema)
	if err != nil {
		return nil, err
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Analyzer{provider: p, prompts: prompts, validator: v, schema: schema, loc: loc}, nil
}

// ErrProvider wraps a provider failure (timeout, process error). The caller
// may retry later; it is not the model's fault.
var ErrProvider = errors.New("analysis: provider failure")

// Analyze runs the triage with one corrective retry on invalid output.
func (a *Analyzer) Analyze(ctx context.Context, in Input) (*Outcome, error) {
	system := SystemPrompt(a.prompts.TriageSystem, in.Profile, in.Extra)
	prompt := TriagePrompt(in, a.loc)
	out := &Outcome{PromptHash: hashOf(system, string(a.schema))}
	for n := 1; n <= 2; n++ {
		resp, err := a.provider.Complete(ctx, llm.Request{Task: config.TaskTriage, System: system, Prompt: prompt, Schema: a.schema})
		if err != nil {
			out.Attempts = append(out.Attempts, Attempt{N: n, Status: "error", Error: err.Error()})
			return out, fmt.Errorf("%w: %w", ErrProvider, err)
		}
		out.Model = resp.Model
		t, problems := a.check(resp.Output, in)
		if len(problems) == 0 {
			out.Attempts = append(out.Attempts, Attempt{N: n, Status: "ok", Response: resp})
			out.Triage = t
			return out, nil
		}
		out.Attempts = append(out.Attempts, Attempt{N: n, Status: "invalid", Error: strings.Join(problems, "; "), Response: resp})
		prompt = TriagePrompt(in, a.loc) + "\n\nTa réponse précédente a été rejetée par le validateur :\n- " +
			strings.Join(problems, "\n- ") + "\nRenvoie un objet JSON corrigé, conforme au schéma."
	}
	return out, nil
}

// check validates the model output and applies Go-side guarantees.
func (a *Analyzer) check(raw json.RawMessage, in Input) (*Triage, []string) {
	if err := a.validator.Validate(raw); err != nil {
		return nil, []string{"schéma : " + oneLine(err.Error(), 500)}
	}
	var t Triage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, []string{"décodage : " + err.Error()}
	}
	var problems []string
	if utf8.RuneCountInString(t.Summary) > 400 {
		problems = append(problems, "summary dépasse 400 caractères")
	}
	if strings.TrimSpace(t.Summary) == "" {
		problems = append(problems, "summary est vide")
	}
	now := in.Now.In(a.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, a.loc)
	body := normForMatch(in.Email.Text + "\n" + in.Email.Subject)
	for i := range t.Actions {
		act := &t.Actions[i]
		act.Text = strings.TrimSpace(act.Text)
		if act.DueTime != "" && act.DueDate == "" {
			problems = append(problems, fmt.Sprintf("actions[%d] : due_time sans due_date", i))
		}
		if act.DueDate != "" {
			d, err := time.ParseInLocation("2006-01-02", act.DueDate, a.loc)
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("actions[%d] : due_date %q invalide", i, act.DueDate))
			case d.Before(today.AddDate(0, 0, -1)) || d.After(today.AddDate(1, 0, 0)):
				// Implausible: drop the date rather than invent a reminder.
				act.DueDate, act.DueTime, act.DateDropped = "", "", true
			}
		}
		q := normForMatch(act.SourceQuote)
		act.QuoteVerified = len(q) >= 3 && strings.Contains(body, q)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	t.InjectionDetected = DetectInjection(in.Email.Text) || DetectInjection(in.Email.Subject)
	return &t, nil
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

var spaceRe = regexp.MustCompile(`\s+`)

// normForMatch makes quote matching tolerant to whitespace, case and
// typographic quotes, nothing else.
func normForMatch(s string) string {
	s = strings.NewReplacer("’", "'", "‘", "'", "“", "\"", "”", "\"", "«", "\"", "»", "\"", " ", " ", " ", " ").Replace(s)
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.ToLower(strings.TrimSpace(strings.Trim(s, " .…\"'")))
}

func oneLine(s string, n int) string {
	s = spaceRe.ReplaceAllString(s, " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
