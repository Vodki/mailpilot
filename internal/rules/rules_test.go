package rules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = `
rules:
  - name: patron
    when: { from: "Chef@Example.com" }
    then: { priority: urgent, notify: always }
  - name: outils
    when: { from_domain: [notifications.example.com, "@ci.example.org"] }
    then: { skip_llm: true, digest_only: true }
  - name: factures
    when: { subject_regex: "(?i)facture|invoice", has_attachment: true }
    then: { extra_instructions: "Extraire le montant.", todoist: confirm, continue: true }
  - name: compta
    when: { from_domain: compta.example }
    then: { priority: haute, todoist: auto, extra_instructions: "Mentionner le fournisseur.", continue: true }
  - name: copie
    when: { addressed: cc }
    then: { notify: never, priority: basse }
  - name: liste
    when: { list_id: newsletter.example }
    then: { ignore: true }
`

func TestEval(t *testing.T) {
	s, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		f    Facts
		want Effects
	}{
		{"first match stops", Facts{From: "chef@example.com", Addressed: "cc"},
			Effects{Priority: "urgent", Notify: "always", Rules: []string{"patron"}}},
		{"subdomain and list", Facts{From: "bot@eu.ci.example.org", Addressed: "direct"},
			Effects{SkipLLM: true, DigestOnly: true, Rules: []string{"outils"}}},
		{"continue fills only unset", Facts{From: "x@compta.example", Subject: "Facture 42", HasAttachment: true, Addressed: "cc"},
			Effects{Priority: "haute", Notify: "never", Todoist: "confirm", Extra: "Extraire le montant.\nMentionner le fournisseur.",
				Rules: []string{"factures", "compta", "copie"}}},
		{"attachment required", Facts{From: "x@y.z", Subject: "facture", Addressed: "direct"}, Effects{}},
		{"list id", Facts{From: "n@x", ListID: "<produit.newsletter.example>", Addressed: "none"},
			Effects{Ignore: true, Rules: []string{"liste"}}},
		{"domain must not match suffix without dot", Facts{From: "a@xcompta.example", Addressed: "direct"}, Effects{}},
	}
	for _, c := range cases {
		if got := s.Eval(c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	bad := map[string]string{
		"unknown field": "rules:\n  - name: a\n    when: { form: x }\n    then: { ignore: true }\n",
		"bad regex":     "rules:\n  - name: a\n    when: { subject_regex: \"(\" }\n    then: {}\n",
		"bad priority":  "rules:\n  - name: a\n    when: { from: x }\n    then: { priority: top }\n",
		"empty when":    "rules:\n  - name: a\n    then: { ignore: true }\n",
		"duplicate":     "rules:\n  - name: a\n    when: { from: x }\n  - name: a\n    when: { from: y }\n",
		"bad addressed": "rules:\n  - when: { addressed: bcc }\n",
	}
	for name, y := range bad {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadMissingAndExample(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil || len(s.Rules) != 0 {
		t.Fatalf("missing file: %v", err)
	}
	ex, err := Load(filepath.Join("..", "..", "config.example", "rules.yaml"))
	if err != nil {
		t.Fatalf("example rules must stay valid: %v", err)
	}
	if len(ex.Rules) == 0 {
		t.Fatal("example has no rules")
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "config.example", "rules.yaml"))
	if !strings.Contains(string(raw), "continue") {
		t.Fatal("example should document continue")
	}
}
