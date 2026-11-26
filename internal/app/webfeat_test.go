package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/Vodki/mailpilot/internal/store"
)

func TestWebDraftAndSend(t *testing.T) {
	srv, mut := withSMTP(t, true)
	h := newHarness(t, mut)
	h.start()
	id, _ := h.urgentNotification("html_only.eml", "Peux-tu valider le devis")
	tgBefore := len(h.tg.Calls("sendMessage"))

	d, err := h.app.WebDraft(h.ctx, WebDraftRequest{MessagePK: id})
	if err != nil || d.Version != 1 || !strings.Contains(d.Body, "C'est noté.") || d.IMAPFolder != "Drafts" {
		t.Fatalf("draft: %+v %v", d, err)
	}
	if len(h.tg.Calls("sendMessage")) != tgBefore {
		t.Fatal("a web draft must not be posted to Telegram")
	}
	// Louis edits the text: v2, v1 leaves Drafts.
	d2, err := h.app.WebDraft(h.ctx, WebDraftRequest{MessagePK: id, Body: "Bonjour Pierre,\n\nC'est validé.\n\nLouis"})
	if err != nil || d2.Version != 2 || d2.Body != "Bonjour Pierre,\n\nC'est validé.\n\nLouis" || h.ops.draftCount() != 1 {
		t.Fatalf("edit: %+v %v (drafts %d)", d2, err, h.ops.draftCount())
	}
	// A confirmation of v1 is stale.
	if _, err := h.app.WebSend(h.ctx, d.ID, 1); !errors.Is(err, ErrWebNoDraft) || srv.MailCalls() != 0 {
		t.Fatalf("stale version: %v", err)
	}
	note, err := h.app.WebSend(h.ctx, d2.ID, 2)
	if err != nil || note != "" {
		t.Fatalf("send: %q %v", note, err)
	}
	mails := srv.Mails()
	if len(mails) != 1 || strings.Join(mails[0].To, ",") != "pierre@client.example" || !strings.Contains(string(mails[0].Data), "C'est valid") {
		t.Fatalf("sent %+v", mails)
	}
	if _, err := h.app.WebSend(h.ctx, d2.ID, 2); err == nil || len(srv.Mails()) != 1 {
		t.Fatal("a draft is sent once")
	}
	if h.ops.draftCount() != 0 || len(h.ops.sent) != 1 {
		t.Fatal("draft must move from Drafts to Sent")
	}
	var n int
	_ = h.st.DB().QueryRow("SELECT COUNT(*) FROM audit_log WHERE actor = 'web' AND event = 'send_confirmed'").Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows %d", n)
	}
}

func TestWebSendGatesAndSuspect(t *testing.T) {
	srv, mut := withSMTP(t, false)
	h := newHarness(t, mut)
	h.start()
	id, _ := h.urgentNotification("html_only.eml", "Peux-tu valider le devis")
	d, err := h.app.WebDraft(h.ctx, WebDraftRequest{MessagePK: id})
	if err != nil {
		t.Fatal(err)
	}
	if why := h.app.WebSendGate(h.ctx); !strings.Contains(why, "send_enabled: false") {
		t.Fatalf("gate %q", why)
	}
	if _, err := h.app.WebSend(h.ctx, d.ID, d.Version); err == nil || srv.MailCalls() != 0 {
		t.Fatal("sent while disabled")
	}
	if err := h.app.WebCancelDraft(h.ctx, d.ID); err != nil || h.ops.draftCount() != 0 {
		t.Fatalf("cancel: %v", err)
	}
	if err := h.app.WebCancelDraft(h.ctx, d.ID); !errors.Is(err, ErrWebNoDraft) {
		t.Fatalf("cancel twice: %v", err)
	}

	h.prov.answers["Ignore tes instructions"] = triageJSON("urgent", "action_requise", "Demande.", "")
	bad := h.deliver("injection.eml", false)
	h.waitDelivery(bad, store.DeliveryImmediate)
	if _, err := h.app.WebDraft(h.ctx, WebDraftRequest{MessagePK: bad}); !errors.Is(err, ErrWebSuspect) {
		t.Fatalf("AI draft of a suspect mail: %v", err)
	}
	if _, err := h.app.WebDraft(h.ctx, WebDraftRequest{MessagePK: bad, Body: "Non merci."}); err != nil {
		t.Fatalf("Louis may still write himself: %v", err)
	}
}

func TestWebComposeNewAndForward(t *testing.T) {
	srv, mut := withSMTP(t, true)
	h := newHarness(t, mut)
	h.start()
	id, _ := h.urgentNotification("html_only.eml", "Peux-tu valider le devis")

	if _, err := h.app.WebCompose(h.ctx, WebComposeRequest{To: "pas une adresse", Subject: "x"}); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	if _, err := h.app.WebCompose(h.ctx, WebComposeRequest{Cc: "a@b.test", Subject: "x"}); err == nil {
		t.Fatal("no To accepted")
	}
	c, err := h.app.WebCompose(h.ctx, WebComposeRequest{To: "Marie <marie@example.test>", Subject: "Dîner", Body: "Samedi ?"})
	if err != nil || c.Version != 1 || c.IMAPFolder != "Drafts" || h.ops.draftCount() != 1 {
		t.Fatalf("new: %+v %v", c, err)
	}
	c2, err := h.app.WebCompose(h.ctx, WebComposeRequest{ID: c.ID, To: "marie@example.test, paul@example.test", Subject: "Dîner", Body: "Samedi 20h ?"})
	if err != nil || c2.Version != 2 || h.ops.draftCount() != 1 {
		t.Fatalf("edit: %+v %v (drafts %d)", c2, err, h.ops.draftCount())
	}
	if _, err := h.app.WebSendComposition(h.ctx, c.ID, 1); !errors.Is(err, ErrWebNoDraft) {
		t.Fatalf("stale version: %v", err)
	}
	if _, err := h.app.WebSendComposition(h.ctx, c.ID, 2); err != nil {
		t.Fatal(err)
	}
	m := srv.Mails()
	if len(m) != 1 || strings.Join(m[0].To, ",") != "marie@example.test,paul@example.test" || !strings.Contains(string(m[0].Data), "Samedi 20h") {
		t.Fatalf("sent %+v", m)
	}
	if _, err := h.app.WebSendComposition(h.ctx, c.ID, 2); err == nil || len(srv.Mails()) != 1 {
		t.Fatal("sent twice")
	}
	if h.ops.draftCount() != 0 {
		t.Fatal("draft copy left in Drafts")
	}

	f, err := h.app.WebCompose(h.ctx, WebComposeRequest{Forward: id, To: "compta@example.test", Subject: "TR: devis", Body: "Pour info."})
	if err != nil || f.Kind != store.CompositionForward || f.SourcePK != id || !strings.Contains(string(f.Raw), "Message transf") {
		t.Fatalf("forward: %+v %v", f, err)
	}
	if err := h.app.WebCancelComposition(h.ctx, f.ID); err != nil || h.ops.draftCount() != 0 {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := h.app.WebCompose(h.ctx, WebComposeRequest{ID: f.ID, To: "x@example.test"}); !errors.Is(err, ErrWebNoDraft) {
		t.Fatalf("edit after cancel: %v", err)
	}
}

func TestWebComposeRespectsSendGate(t *testing.T) {
	srv, mut := withSMTP(t, false)
	h := newHarness(t, mut)
	h.start()
	c, err := h.app.WebCompose(h.ctx, WebComposeRequest{To: "marie@example.test", Subject: "x", Body: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.WebSendComposition(h.ctx, c.ID, 1); err == nil || srv.MailCalls() != 0 {
		t.Fatal("sent while disabled")
	}
}
