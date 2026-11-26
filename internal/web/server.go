// Package web serves the mailbox interface: the inbox as a normal webmail,
// each mail with its AI analysis (priority, summary, actions, warnings).
// Browsing (any GET) changes nothing in the mailbox: reads use EXAMINE and
// BODY.PEEK. Only the buttons Louis presses (POST with the CSRF token) mark
// read, move, archive or put in the trash, and nothing is ever deleted for
// good.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps are the server's collaborators.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Mailbox  Mailbox  // nil: everything shows as read and actions are off
	Composer Composer // nil: no replies, sends or reminder buttons
	Log      *slog.Logger
	Version  string
	Now      func() time.Time
}

// Server is the web interface.
type Server struct {
	cfg     *config.Config
	st      *store.Store
	mb      Mailbox
	comp    Composer
	log     *slog.Logger
	version string
	now     func() time.Time
	tpl     *template.Template
	limiter *limiter
	secure  bool // Secure cookies (HTTPS)

	mu        sync.Mutex
	unseenAt  time.Time
	unseenVal uint32
	unseenSet map[uint32]bool
	unseenOK  bool

	foldersAt  time.Time
	folderList []Folder
	foldersOK  bool
}

// New builds the server; templates are parsed once here.
func New(d Deps) (*Server, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: d.Config, st: d.Store, mb: d.Mailbox, comp: d.Composer, log: d.Log.With("component", "web"),
		version: d.Version, now: d.Now, tpl: tpl, limiter: newLimiter()}
	if u, err := url.Parse(d.Config.Web.PublicURL); err == nil && u.Scheme == "https" {
		s.secure = true
	}
	return s, nil
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Web.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Web.Listen)
	if err != nil {
		return err
	}
	s.log.Info("web interface listening", "addr", ln.Addr().String())
	go func() {
		<-ctx.Done()
		// ctx is done: shutdown needs a fresh deadline of its own.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go s.housekeeping(ctx)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.st.PurgeWebSessions(ctx, s.now()); err == nil && n > 0 {
				s.log.Info("expired web sessions purged", "count", n)
			}
		}
	}
}

// Handler returns the routes wrapped in the security headers.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(time.Hour, files)))
	mux.HandleFunc("GET /manifest.webmanifest", s.serveRootFile("manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /sw.js", s.serveRootFile("sw.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.auth(s.logout))
	mux.HandleFunc("GET /{$}", s.auth(func(w http.ResponseWriter, r *http.Request, _ *store.WebSession) {
		http.Redirect(w, r, "/v/inbox", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /v/{view}", s.auth(s.viewPage))
	mux.HandleFunc("GET /events", s.auth(s.events))
	mux.HandleFunc("GET /dossier", s.auth(s.folderPage))
	mux.HandleFunc("POST /action", s.auth(s.action))
	mux.HandleFunc("POST /brouillon", s.auth(s.draftAction))
	mux.HandleFunc("POST /envoi/preparer", s.auth(s.sendPrepare))
	mux.HandleFunc("POST /envoi/confirmer", s.auth(s.sendConfirm))
	mux.HandleFunc("POST /rappel", s.auth(s.todoAction))
	return securityHeaders(mux)
}

func cacheFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age="+itoa(int64(d/time.Second)))
		h.ServeHTTP(w, r)
	})
}

func (s *Server) serveRootFile(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		b, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			http.NotFound(w, nil)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	}
}

// securityHeaders applies a strict policy: no inline script or style, no
// framing, no referrer, no external resource of any kind.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; manifest-src 'self'; worker-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin") // nothing leaks to other sites; Origin stays set on our POSTs
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// ---------- sessions ----------

const cookieName = "mp_session"

type authedHandler func(http.ResponseWriter, *http.Request, *store.WebSession)

// auth requires a live session; pages redirect to /login, other requests
// get 401.
func (s *Server) auth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		var ws *store.WebSession
		if err == nil && len(c.Value) == 64 {
			ws, err = s.st.GetWebSession(r.Context(), tokenHash(c.Value), s.now())
		} else {
			err = store.ErrNotFound
		}
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Error("session lookup", "err", err)
				http.Error(w, "erreur interne", http.StatusInternalServerError)
				return
			}
			if r.Method == http.MethodGet && r.Header.Get("X-Partial") == "" && r.URL.Path != "/events" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "session expirée", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && !s.sameOrigin(r) {
			http.Error(w, "requête refusée", http.StatusForbidden)
			return
		}
		if s.now().Sub(ws.LastSeen) > time.Minute {
			_ = s.st.TouchWebSession(r.Context(), ws.TokenHash, s.now())
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r, ws)
	}
}

// sameOrigin rejects cross-site form posts (with SameSite=Strict and the
// CSRF token, a third layer).
func (s *Server) sameOrigin(r *http.Request) bool {
	// Modern browsers say where the request comes from: only our own pages
	// (or a typed URL, "none") may post.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser form post (curl, tests); the CSRF token still applies
	}
	u, err := url.Parse(origin)
	return err == nil && origin != "null" && strings.EqualFold(u.Host, r.Host)
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) setCookie(w http.ResponseWriter, value string, maxAge int) {
	// Secure is set whenever the interface is served over HTTPS (public_url);
	// on the loopback interface it is plain HTTP and the cookie stays local.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure depends on public_url, see above

		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("e") != ""})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "requête refusée", http.StatusForbidden)
		return
	}
	key := clientKey(r)
	now := s.now()
	if !s.limiter.allowed(key, now) {
		s.log.Warn("web login throttled", "client", key)
		http.Error(w, "Trop de tentatives. Réessaie dans 15 minutes.", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	ok, err := VerifyPassword(s.cfg.Web.PasswordHash.Reveal(), r.PostFormValue("password"))
	if err != nil {
		s.log.Error("web password hash", "err", err)
	}
	if !ok {
		s.limiter.fail(key, now)
		_ = s.st.Audit(r.Context(), "web", "login_failed", key)
		s.log.Warn("web login failed", "client", key)
		http.Redirect(w, r, "/login?e=1", http.StatusSeeOther)
		return
	}
	s.limiter.reset(key)
	token := newToken()
	ws := store.WebSession{TokenHash: tokenHash(token), CSRF: newToken(), ExpiresAt: now.Add(s.cfg.Web.SessionTTL)}
	if err := s.st.CreateWebSession(r.Context(), ws, now); err != nil {
		s.log.Error("create web session", "err", err)
		http.Error(w, "erreur interne", http.StatusInternalServerError)
		return
	}
	_ = s.st.Audit(r.Context(), "web", "login", key)
	s.log.Info("web login", "client", key)
	s.setCookie(w, token, int(s.cfg.Web.SessionTTL/time.Second))
	http.Redirect(w, r, "/v/inbox", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, ws *store.WebSession) {
	if !s.checkCSRF(r, ws) {
		http.Error(w, "jeton CSRF invalide", http.StatusForbidden)
		return
	}
	_ = s.st.DeleteWebSession(r.Context(), ws.TokenHash)
	_ = s.st.Audit(r.Context(), "web", "logout", clientKey(r))
	s.setCookie(w, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) checkCSRF(r *http.Request, ws *store.WebSession) bool {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<16)
	got := r.PostFormValue("csrf")
	return len(got) == len(ws.CSRF) && constantEq(got, ws.CSRF)
}

// ---------- live updates ----------

// events streams a "change" event whenever the lists would render
// differently (Server-Sent Events, checked every 2 s).
func (s *Server) events(w http.ResponseWriter, r *http.Request, _ *store.WebSession) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming non pris en charge", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	last, _ := s.st.ChangeVersion(r.Context())
	lastFlags := s.flagsFingerprint(r.Context())
	_, _ = w.Write([]byte("retry: 3000\n\n"))
	fl.Flush()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	beat := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			beat++
			if beat%30 == 0 {
				// Every minute: mails read, moved or deleted from another
				// client (this also hides the ones that left the inbox).
				if fp := s.flagsFingerprint(r.Context()); fp != lastFlags {
					lastFlags = fp
					if _, err := w.Write([]byte("event: change\ndata: 1\n\n")); err != nil {
						return
					}
					fl.Flush()
				}
			}
			v, err := s.st.ChangeVersion(r.Context())
			if err != nil {
				continue
			}
			if v != last {
				last = v
				s.invalidateStatus() // a new mail is unread: refresh the flags
				if _, err := w.Write([]byte("event: change\ndata: 1\n\n")); err != nil {
					return
				}
				fl.Flush()
				continue
			}
			if beat%12 == 0 {
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return
				}
				fl.Flush()
			}
		}
	}
}

// flagsFingerprint summarises the unread state of the inbox.
func (s *Server) flagsFingerprint(ctx context.Context) string {
	val, set := s.inboxStatus(ctx)
	var sum uint64
	for u := range set {
		sum += uint64(u) * 2654435761
	}
	return strconv.FormatUint(uint64(val), 10) + ":" + strconv.Itoa(len(set)) + ":" + strconv.FormatUint(sum, 10)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render", "template", name, "err", err)
	}
}
