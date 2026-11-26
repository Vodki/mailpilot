// Package rules implements rules.yaml, the deterministic pre-filter written
// by Louis. Rules are evaluated top to bottom; the first match applies and
// stops evaluation unless it sets `continue: true`, in which case later
// matches only fill effects that are still unset (extra_instructions are
// concatenated). Rules only see header-derived facts, never the body.
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// Facts are the header-derived properties a rule can test.
type Facts struct {
	From          string // address, lower case
	Subject       string
	HasAttachment bool
	Addressed     string // direct | cc | none
	ListID        string
}

// Effects is the merged outcome of the matching rules.
type Effects struct {
	Ignore     bool
	SkipLLM    bool
	DigestOnly bool
	Priority   string // urgent | haute | normale | basse
	Notify     string // always | never | auto
	Extra      string
	Todoist    string // auto | confirm | off
	AutoDraft  bool
	Rules      []string // names of the rules that matched
}

// StringList accepts a scalar or a list in YAML.
type StringList []string

// UnmarshalYAML implements yaml.BytesUnmarshaler.
func (s *StringList) UnmarshalYAML(b []byte) error {
	var one string
	if err := yaml.Unmarshal(b, &one); err == nil {
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := yaml.Unmarshal(b, &many); err != nil {
		return errors.New("attendu : une chaîne ou une liste de chaînes")
	}
	*s = many
	return nil
}

// When is a rule's condition; all set fields must match.
type When struct {
	From          StringList `yaml:"from"`
	FromDomain    StringList `yaml:"from_domain"`
	SubjectRegex  string     `yaml:"subject_regex"`
	HasAttachment *bool      `yaml:"has_attachment"`
	Addressed     string     `yaml:"addressed"`
	ListID        StringList `yaml:"list_id"`
}

// Then is a rule's effect. Pointers distinguish "unset" from false.
type Then struct {
	Ignore            *bool  `yaml:"ignore"`
	SkipLLM           *bool  `yaml:"skip_llm"`
	Priority          string `yaml:"priority"`
	Notify            string `yaml:"notify"`
	DigestOnly        *bool  `yaml:"digest_only"`
	ExtraInstructions string `yaml:"extra_instructions"`
	Todoist           string `yaml:"todoist"`
	AutoDraft         *bool  `yaml:"auto_draft"`
	Continue          bool   `yaml:"continue"`
}

// Rule is one entry of rules.yaml.
type Rule struct {
	Name string `yaml:"name"`
	When When   `yaml:"when"`
	Then Then   `yaml:"then"`

	subject *regexp.Regexp
}

// Set is a validated, immutable ruleset.
type Set struct {
	Rules []Rule `yaml:"rules"`
}

// Load reads and validates a rules file. A missing file is an empty set.
func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied rules path
	if errors.Is(err, os.ErrNotExist) {
		return &Set{}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates rules.
func Parse(data []byte) (*Set, error) {
	var s Set
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.NewDecoder(bytes.NewReader(data), yaml.DisallowUnknownField()).Decode(&s); err != nil {
			return nil, err
		}
	}
	var errs []error
	names := map[string]bool{}
	for i := range s.Rules {
		r := &s.Rules[i]
		where := fmt.Sprintf("règle %d", i+1)
		if r.Name == "" {
			r.Name = fmt.Sprintf("règle-%d", i+1)
		} else {
			where += " (" + r.Name + ")"
		}
		if names[r.Name] {
			errs = append(errs, fmt.Errorf("%s : nom en double", where))
		}
		names[r.Name] = true
		w := r.When
		if len(w.From) == 0 && len(w.FromDomain) == 0 && w.SubjectRegex == "" && w.HasAttachment == nil && w.Addressed == "" && len(w.ListID) == 0 {
			errs = append(errs, fmt.Errorf("%s : `when` est vide (une règle doit avoir au moins une condition)", where))
		}
		if w.SubjectRegex != "" {
			re, err := regexp.Compile(w.SubjectRegex)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s : subject_regex invalide : %w", where, err))
			}
			r.subject = re
		}
		if !oneOf(w.Addressed, "", "direct", "cc", "none") {
			errs = append(errs, fmt.Errorf("%s : addressed doit valoir direct, cc ou none", where))
		}
		t := r.Then
		if !oneOf(t.Priority, "", "urgent", "haute", "normale", "basse") {
			errs = append(errs, fmt.Errorf("%s : priority doit valoir urgent, haute, normale ou basse", where))
		}
		if !oneOf(t.Notify, "", "always", "never", "auto") {
			errs = append(errs, fmt.Errorf("%s : notify doit valoir always, never ou auto", where))
		}
		if !oneOf(t.Todoist, "", "auto", "confirm", "off") {
			errs = append(errs, fmt.Errorf("%s : todoist doit valoir auto, confirm ou off", where))
		}
		if len(t.ExtraInstructions) > 2000 {
			errs = append(errs, fmt.Errorf("%s : extra_instructions dépasse 2000 caractères", where))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &s, nil
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func (r *Rule) matches(f Facts) bool {
	w := r.When
	from := strings.ToLower(f.From)
	if len(w.From) > 0 && !anyFold(w.From, func(v string) bool { return strings.EqualFold(strings.TrimSpace(v), from) }) {
		return false
	}
	if len(w.FromDomain) > 0 {
		_, dom, _ := strings.Cut(from, "@")
		if !anyFold(w.FromDomain, func(v string) bool {
			v = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "@"))
			return dom == v || strings.HasSuffix(dom, "."+v)
		}) {
			return false
		}
	}
	if r.subject != nil && !r.subject.MatchString(f.Subject) {
		return false
	}
	if w.HasAttachment != nil && *w.HasAttachment != f.HasAttachment {
		return false
	}
	if w.Addressed != "" && w.Addressed != f.Addressed {
		return false
	}
	if len(w.ListID) > 0 && !anyFold(w.ListID, func(v string) bool {
		return f.ListID != "" && strings.Contains(strings.ToLower(f.ListID), strings.ToLower(strings.TrimSpace(v)))
	}) {
		return false
	}
	return true
}

func anyFold(vals []string, f func(string) bool) bool {
	for _, v := range vals {
		if f(v) {
			return true
		}
	}
	return false
}

// Eval returns the merged effects for a message.
func (s *Set) Eval(f Facts) Effects {
	var e Effects
	if s == nil {
		return e
	}
	var setIgnore, setSkip, setDigest, setDraft bool
	for i := range s.Rules {
		r := &s.Rules[i]
		if !r.matches(f) {
			continue
		}
		t := r.Then
		e.Rules = append(e.Rules, r.Name)
		fillBool(&e.Ignore, &setIgnore, t.Ignore)
		fillBool(&e.SkipLLM, &setSkip, t.SkipLLM)
		fillBool(&e.DigestOnly, &setDigest, t.DigestOnly)
		fillBool(&e.AutoDraft, &setDraft, t.AutoDraft)
		if e.Priority == "" {
			e.Priority = t.Priority
		}
		if e.Notify == "" {
			e.Notify = t.Notify
		}
		if e.Todoist == "" {
			e.Todoist = t.Todoist
		}
		if x := strings.TrimSpace(t.ExtraInstructions); x != "" {
			if e.Extra != "" {
				e.Extra += "\n"
			}
			e.Extra += x
		}
		if !t.Continue {
			break
		}
	}
	return e
}

func fillBool(dst *bool, set *bool, v *bool) {
	if *set || v == nil {
		return
	}
	*dst, *set = *v, true
}
