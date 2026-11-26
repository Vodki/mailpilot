package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/store"
)

// Composer writes and sends replies, and handles reminders. The production
// implementation (cmd/mailpilot) is the app's own flow, with the same
// gates as Telegram: sending off by default, pause, hourly limit,
// recipients recomputed from the headers, one send per draft.
type Composer interface {
	Draft(ctx context.Context, r DraftRequest) (draftID int64, err error)
	CancelDraft(ctx context.Context, draftID int64) error
	SendGate(ctx context.Context) string
	Send(ctx context.Context, draftID int64, version int) (note string, err error)
	Todo(ctx context.Context, actionID int64, add bool) error
	Compose(ctx context.Context, r ComposeRequest) (id int64, err error)
	CancelComposition(ctx context.Context, id int64) error
	SendComposition(ctx context.Context, id int64, version int) (note string, err error)
}

// DraftRequest asks for a new version of the reply to a stored message.
type DraftRequest struct {
	MessagePK    int64
	Instructions string
	Body         string // Louis's own text: no AI
	ReplyAll     bool
	Regenerate   bool
}

const (
	cbWebSend      = "web_send"
	sendConfirmTTL = 10 * time.Minute
	maxBody        = 100_000
)

type draftVM struct {
	ID           int64
	Version      int
	To, Cc       string
	Subject      string
	Body         string
	Instructions string
	ReplyAll     bool
	InDrafts     bool
	SendBlocked  string // why sending is refused now, "" if allowed
}

type confirmVM struct {
	Title            string
	Quoted, Forward  bool // a quoted original / the forwarded mail follows the text
	Token            string
	To, Cc, Subject  string
	Body             string
	Version          int
	ReplyToElsewhere string
}

func addrText(l []store.Address) string {
	parts := make([]string, 0, len(l))
	for _, a := range l {
		if a.Name != "" {
			parts = append(parts, a.Name+" <"+a.Email+">")
		} else {
			parts = append(parts, a.Email)
		}
	}
	return strings.Join(parts, ", ")
}

// fillDraft adds the active draft (if any) to a stored mail's pane.
func (s *Server) fillDraft(ctx context.Context, p *paneVM) error {
	if s.comp == nil || p.ID == 0 || p.ByLoc {
		return nil
	}
	p.CanReply = !p.Missing
	d, err := s.st.ActiveDraft(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	p.Draft = &draftVM{ID: d.ID, Version: d.Version, To: addrText(d.To), Cc: addrText(d.Cc), Subject: d.Subject,
		Body: d.Body, Instructions: d.Instructions, ReplyAll: d.ReplyAll, InDrafts: d.IMAPFolder != "",
		SendBlocked: s.comp.SendGate(ctx)}
	return nil
}

// draftAction: new draft (reply / reply all), save Louis's text,
// regenerate with instructions, or drop the draft.
func (s *Server) draftAction(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	if s.comp == nil {
		s.actionReply(w, r, http.StatusServiceUnavailable, actionResult{Message: "La rédaction est indisponible."})
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	op := r.PostFormValue("op")
	body := strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n")
	instr := strings.TrimSpace(r.PostFormValue("instructions"))
	if len(body) > maxBody || len(instr) > 2000 {
		s.actionReply(w, r, http.StatusRequestEntityTooLarge, actionResult{Message: "Texte trop long."})
		return
	}
	// An AI draft takes a few seconds, a slow model up to a minute or two.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	req := DraftRequest{MessagePK: id, ReplyAll: r.PostFormValue("all") == "1"}
	var err error
	msg := ""
	switch op {
	case "nouveau": // reply or reply all, written by the AI
		req.Instructions = instr
		_, err = s.comp.Draft(ctx, req)
		msg = "Brouillon prêt, enregistré dans Brouillons"
	case "enregistrer": // Louis's own text (first version or an edit)
		if strings.TrimSpace(body) == "" {
			s.actionReply(w, r, http.StatusBadRequest, actionResult{Message: "Le brouillon est vide."})
			return
		}
		req.Body = body
		_, err = s.comp.Draft(ctx, req)
		msg = "Brouillon enregistré"
	case "regenerer":
		req.Instructions, req.Regenerate = instr, true
		_, err = s.comp.Draft(ctx, req)
		msg = "Nouvelle version prête"
	case "abandonner":
		draftID, _ := strconv.ParseInt(r.PostFormValue("draft"), 10, 64)
		err = s.comp.CancelDraft(ctx, draftID)
		msg = "Brouillon supprimé (et retiré du dossier Brouillons)"
	default:
		http.Error(w, "action inconnue", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Warn("draft action failed", "op", op, "message_pk", id, "err", err)
		s.actionReply(w, r, http.StatusConflict, actionResult{Message: "Brouillon impossible : " + err.Error()})
		return
	}
	s.actionReply(w, r, http.StatusOK, actionResult{OK: true, Message: msg})
}

// sendPrepare is the first click on "Envoyer": it shows exactly what will
// leave, to whom, with a single-use confirmation valid 10 minutes.
func (s *Server) sendPrepare(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	if s.comp == nil {
		http.Error(w, "envoi indisponible", http.StatusServiceUnavailable)
		return
	}
	draftID, _ := strconv.ParseInt(r.PostFormValue("draft"), 10, 64)
	version, _ := strconv.Atoi(r.PostFormValue("version"))
	ctx := r.Context()
	if why := s.comp.SendGate(ctx); why != "" {
		http.Error(w, why, http.StatusConflict)
		return
	}
	var vm confirmVM
	if r.PostFormValue("kind") == "c" {
		c, err := s.st.GetComposition(ctx, draftID)
		if err != nil || c.Status != "active" || c.Version != version {
			http.Error(w, "Ce message a changé : relis-le avant d'envoyer.", http.StatusConflict)
			return
		}
		token, err := s.st.CreateCallback(ctx, cbWebSendNew, 0, c.ID, strconv.Itoa(c.Version), s.now().Add(sendConfirmTTL))
		if err != nil {
			s.fail(w, "send token", err)
			return
		}
		vm = confirmVM{Token: token, To: addrText(c.To), Cc: addrText(c.Cc), Subject: c.Subject, Body: c.Body, Version: c.Version,
			Title: "Envoyer ce message ?", Forward: c.Kind == store.CompositionForward}
	} else {
		d, err := s.st.GetDraft(ctx, draftID)
		if err != nil || d.Status != store.DraftActive || d.Version != version {
			http.Error(w, "Ce brouillon a changé : relis-le avant d'envoyer.", http.StatusConflict)
			return
		}
		m, err := s.st.GetMessage(ctx, d.MessagePK)
		if err != nil {
			http.Error(w, "mail introuvable", http.StatusNotFound)
			return
		}
		token, err := s.st.CreateCallback(ctx, cbWebSend, d.MessagePK, d.ID, strconv.Itoa(d.Version), s.now().Add(sendConfirmTTL))
		if err != nil {
			s.fail(w, "send token", err)
			return
		}
		vm = confirmVM{Token: token, To: addrText(d.To), Cc: addrText(d.Cc), Subject: d.Subject, Body: d.Body, Version: d.Version,
			Title: "Envoyer cette réponse ?", Quoted: true}
		if len(m.ReplyTo) > 0 && !strings.EqualFold(m.ReplyTo[0].Email, m.From.Email) {
			vm.ReplyToElsewhere = "La réponse part vers l'adresse de réponse indiquée par l'expéditeur (" + m.ReplyTo[0].Email +
				"), pas vers " + m.From.Email + "."
		}
	}
	s.render(w, "confirm", struct {
		CSRF string
		C    confirmVM
	}{ws.CSRF, vm})
}

// sendConfirm is the second click. The token is single-use and every gate
// is checked again by the Composer.
func (s *Server) sendConfirm(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	if s.comp == nil {
		s.actionReply(w, r, http.StatusServiceUnavailable, actionResult{Message: "Envoi indisponible."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	c, err := s.st.ConsumeCallback(ctx, r.PostFormValue("token"), s.now())
	if err != nil || (c.Action != cbWebSend && c.Action != cbWebSendNew) {
		msg := "Confirmation invalide : recommence depuis « Envoyer »."
		if errors.Is(err, store.ErrCallbackExpired) {
			msg = "Confirmation expirée (10 minutes) : recommence depuis « Envoyer »."
		}
		s.actionReply(w, r, http.StatusConflict, actionResult{Message: msg})
		return
	}
	version, _ := strconv.Atoi(c.Arg)
	send, done, next := s.comp.Send, "Réponse envoyée", ""
	if c.Action == cbWebSendNew {
		send, done, next = s.comp.SendComposition, "Message envoyé", "/v/inbox"
	}
	note, err := send(ctx, c.DraftPK, version)
	if err != nil {
		s.log.Warn("web send failed", "draft_id", c.DraftPK, "err", err)
		s.actionReply(w, r, http.StatusConflict, actionResult{Message: "Envoi refusé : " + err.Error() + ". Le brouillon reste dans Brouillons."})
		return
	}
	msg := done
	if note != "" {
		msg += " (" + note + ")"
	}
	s.actionReply(w, r, http.StatusOK, actionResult{OK: true, Message: msg, Redirect: next})
}

// todoAction adds a proposed reminder to Todoist, or dismisses it.
func (s *Server) todoAction(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	if s.comp == nil {
		s.actionReply(w, r, http.StatusServiceUnavailable, actionResult{Message: "Rappels indisponibles."})
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("action"), 10, 64)
	add := r.PostFormValue("op") == "ajouter"
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.comp.Todo(ctx, id, add); err != nil {
		s.actionReply(w, r, http.StatusConflict, actionResult{Message: "Rappel : " + err.Error()})
		return
	}
	msg := "Rappel ignoré"
	if add {
		msg = "Rappel ajouté à Todoist"
	}
	s.actionReply(w, r, http.StatusOK, actionResult{OK: true, Message: msg})
}
