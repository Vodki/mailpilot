package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Vodki/mailpilot/internal/store"
)

// Mailbox actions. Each one is a POST with the session's CSRF token, from
// a button Louis pressed; GET requests never change the mailbox.

type undoVM struct {
	ID     int64  `json:"id,omitempty"`
	Folder string `json:"f,omitempty"`
	Val    uint32 `json:"v,omitempty"`
	UID    uint32 `json:"u,omitempty"`
	Dest   string `json:"dest"`
}

type actionResult struct {
	OK       bool    `json:"ok"`
	Message  string  `json:"message"`
	Gone     bool    `json:"gone,omitempty"` // the mail left the current list
	Undo     *undoVM `json:"undo,omitempty"`
	Redirect string  `json:"redirect,omitempty"` // page to open next
}

var actionOps = map[string]bool{"lu": true, "nonlu": true, "archiver": true, "corbeille": true, "deplacer": true}

func (s *Server) action(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	op := r.PostFormValue("op")
	if !actionOps[op] {
		http.Error(w, "action inconnue", http.StatusBadRequest)
		return
	}
	if s.mb == nil {
		s.actionReply(w, r, http.StatusServiceUnavailable, actionResult{Message: "Les actions sur la boîte sont indisponibles."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	id, at, err := s.actionTarget(ctx, r)
	if err != nil {
		s.actionReply(w, r, http.StatusNotFound, actionResult{Message: "Mail introuvable."})
		return
	}
	var res actionResult
	switch op {
	case "lu", "nonlu":
		seen := op == "lu"
		err = s.mb.SetSeen(ctx, at, seen)
		if err == nil {
			s.noteSeen(at, seen)
			res.Message = map[bool]string{true: "Marqué comme lu", false: "Marqué comme non lu"}[seen]
		}
	default:
		var to Target
		switch op {
		case "archiver":
			to.Role = "archive"
		case "corbeille":
			to.Role = "trash"
		default:
			to.Folder = r.PostFormValue("dest")
			if _, ok := s.folderRole(ctx, to.Folder); !ok || to.Folder == "" {
				s.actionReply(w, r, http.StatusBadRequest, actionResult{Message: "Dossier inconnu."})
				return
			}
		}
		var nl *Loc
		nl, err = s.mb.Move(ctx, at, to)
		if err == nil {
			res = s.afterMove(ctx, op, id, at, *nl)
		}
	}
	ref := "uid:" + strconv.FormatUint(uint64(at.UID), 10)
	if id != 0 {
		ref = "message:" + itoa(id)
	}
	switch {
	case err == nil:
		res.OK = true
		_ = s.st.Audit(ctx, "web", "mailbox_"+op, ref)
		s.log.Info("mailbox action", "op", op, "ref", ref)
		s.actionReply(w, r, http.StatusOK, res)
	case errors.Is(err, ErrSameFolder):
		msg := "Ce mail est déjà dans ce dossier."
		if op == "corbeille" {
			msg = "Ce mail est déjà dans la corbeille. mailpilot ne supprime jamais un mail définitivement."
		}
		s.actionReply(w, r, http.StatusConflict, actionResult{Message: msg})
	case errors.Is(err, ErrMoved):
		s.invalidateStatus()
		s.actionReply(w, r, http.StatusConflict, actionResult{Message: "Ce mail a été déplacé ou supprimé depuis un autre logiciel. La liste va se mettre à jour.", Gone: true})
	default:
		s.log.Warn("mailbox action failed", "op", op, "ref", ref, "err", err)
		s.actionReply(w, r, http.StatusBadGateway, actionResult{Message: "Le serveur de mail a refusé l'action : " + err.Error()})
	}
}

// actionTarget finds the message: by stored id (its recorded location), or
// by location in a folder listing (then linked to the stored message at
// that exact place, if any).
func (s *Server) actionTarget(ctx context.Context, r *http.Request) (int64, Loc, error) {
	if id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64); id > 0 {
		m, err := s.st.GetMessage(ctx, id)
		if err != nil {
			return 0, Loc{}, err
		}
		return id, Loc{Folder: m.Mailbox, UIDValidity: m.UIDValidity, UID: m.UID}, nil
	}
	v, _ := strconv.ParseUint(r.PostFormValue("v"), 10, 32)
	u, _ := strconv.ParseUint(r.PostFormValue("u"), 10, 32)
	at := Loc{Folder: r.PostFormValue("f"), UIDValidity: uint32(v), UID: uint32(u)}
	if at.Folder == "" || at.UID == 0 {
		return 0, Loc{}, store.ErrNotFound
	}
	id, err := s.st.IDAt(ctx, at.Folder, at.UIDValidity, at.UID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return 0, Loc{}, err
	}
	return id, at, nil
}

func (s *Server) afterMove(ctx context.Context, op string, id int64, from, to Loc) actionResult {
	if id != 0 {
		if err := s.st.Relocate(ctx, id, to.Folder, to.UIDValidity, to.UID); err != nil {
			s.log.Warn("relocate", "message_pk", id, "err", err)
		}
	}
	s.invalidateStatus()
	s.mu.Lock()
	s.foldersOK = false // the destination may be new to the cache
	s.mu.Unlock()
	res := actionResult{Gone: true}
	role, _ := s.folderRole(ctx, to.Folder)
	title := folderTitle(Folder{Name: to.Folder, Role: role})
	switch {
	case op == "archiver":
		res.Message = "Archivé dans « " + title + " »"
	case op == "corbeille":
		res.Message = "Mis à la corbeille"
	case to.Folder == s.cfg.IMAP.Mailbox:
		res.Message = "Remis dans la boîte de réception"
	default:
		res.Message = "Déplacé dans « " + title + " »"
	}
	if id != 0 {
		res.Undo = &undoVM{ID: id, Dest: from.Folder}
	} else if to.UID != 0 {
		res.Undo = &undoVM{Folder: to.Folder, Val: to.UIDValidity, UID: to.UID, Dest: from.Folder}
	}
	return res
}

// actionReply answers fetch() with JSON, and a plain form post (no
// JavaScript) with a redirect back to the page it came from.
func (s *Server) actionReply(w http.ResponseWriter, r *http.Request, code int, res actionResult) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	if code != http.StatusOK {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(res.Message + "\n"))
		return
	}
	http.Redirect(w, r, safeBack(r.PostFormValue("back")), http.StatusSeeOther) //nolint:gosec // safeBack keeps only our own list pages
}

// safeBack returns the page a form came from, if it is one of our lists.
func safeBack(back string) string {
	u, err := url.Parse(back)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || strings.Contains(back, "\\") ||
		(!strings.HasPrefix(u.Path, "/v/") && u.Path != "/dossier") {
		return "/v/inbox"
	}
	return (&url.URL{Path: u.Path, RawQuery: u.RawQuery}).RequestURI()
}
