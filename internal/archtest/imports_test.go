// Package archtest enforces architectural rules that protect the
// non-negotiable principles, so a refactor cannot silently break them.
package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/Vodki/mailpilot"

// imports returns, for each package of the module, its non-test imports.
func imports(t *testing.T) map[string][]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", `{{.ImportPath}} {{join .Imports " "}}`, module+"/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	res := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 {
			res[f[0]] = f[1:]
		}
	}
	return res
}

// Rule 3: no email leaves without the reply package's confirmation flow, so
// only internal/reply may hold an SMTP client.
func TestOnlyReplyImportsSMTP(t *testing.T) {
	allowed := map[string]bool{module + "/internal/reply": true, module + "/internal/reply/smtptest": true}
	for pkg, imps := range imports(t) {
		for _, imp := range imps {
			if imp == "net/smtp" {
				t.Errorf("%s imports net/smtp; use go-smtp inside internal/reply", pkg)
			}
			if strings.HasPrefix(imp, "github.com/emersion/go-smtp") && !allowed[pkg] {
				t.Errorf("%s imports %s; only internal/reply may", pkg, imp)
			}
		}
	}
}

// Rule 2: the LLM layer must not depend on anything that can act.
func TestLLMHasNoSideEffectImports(t *testing.T) {
	forbidden := []string{
		module + "/internal/reply", module + "/internal/mailbox", module + "/internal/telegram",
		module + "/internal/todoist", module + "/internal/store",
	}
	for pkg, imps := range imports(t) {
		if !strings.HasPrefix(pkg, module+"/internal/llm") {
			continue
		}
		for _, imp := range imps {
			for _, f := range forbidden {
				if imp == f {
					t.Errorf("%s must not import %s", pkg, imp)
				}
			}
		}
	}
}

// Rule: the web interface reaches the mailbox and the outside world only
// through the narrow interfaces cmd/mailpilot hands it (web.Mailbox: read,
// mark read, move; nothing deletes for good), so it imports nothing that can
// send, notify or change a mailbox by itself.
func TestWebActsOnlyThroughItsInterfaces(t *testing.T) {
	forbidden := []string{
		module + "/internal/reply", module + "/internal/mailbox", module + "/internal/telegram",
		module + "/internal/todoist", module + "/internal/app",
	}
	for pkg, imps := range imports(t) {
		if pkg != module+"/internal/web" {
			continue
		}
		for _, imp := range imps {
			for _, f := range forbidden {
				if imp == f {
					t.Errorf("%s must not import %s", pkg, imp)
				}
			}
		}
	}
}
