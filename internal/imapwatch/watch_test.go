package imapwatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/Vodki/mailpilot/internal/config"
	"github.com/Vodki/mailpilot/internal/mailbox"
	"github.com/Vodki/mailpilot/internal/testutil"
)

type memSink struct {
	mu   sync.Mutex
	cur  Cursor
	got  []*Fetched
	seen map[uint32]bool
}

func (s *memSink) LoadCursor(context.Context, string) (Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur, nil
}

func (s *memSink) SaveCursor(_ context.Context, _ string, c Cursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = c
	return nil
}

func (s *memSink) Deliver(_ context.Context, f *Fetched) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[uint32]bool{}
	}
	if !s.seen[f.UID] {
		s.seen[f.UID] = true
		s.got = append(s.got, f)
	}
	return nil
}

func (s *memSink) cursor() Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *memSink) snapshot() []*Fetched {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

type server struct {
	addr string
	user *imapmemserver.User
}

func start(t *testing.T, caps imap.CapSet) *server {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("louis@example.test", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: caps, InsecureAuth: true,
	})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", testutil.SelfSignedTLS(t))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &server{addr: ln.Addr().String(), user: user}
}

func (s *server) add(t *testing.T, id string, at time.Time) {
	t.Helper()
	raw := fmt.Sprintf("From: a@x.test\r\nTo: louis@example.test\r\nSubject: %s\r\nMessage-ID: <%s@x.test>\r\nDate: %s\r\n\r\nCorps %s\r\n",
		id, id, at.Format(time.RFC1123Z), id)
	if _, err := s.user.Append("INBOX", bytes.NewReader([]byte(raw)), &imap.AppendOptions{Time: at}); err != nil {
		t.Fatal(err)
	}
}

func cfg(t *testing.T, addr string) config.IMAPConfig {
	host, port := testutil.SplitAddr(t, addr)
	return config.IMAPConfig{
		Host: host, Port: port, Security: config.SecurityTLS, Username: "louis@example.test",
		Password: config.NewSecret("secret"), Mailbox: "INBOX", InsecureSkipVerify: true,
		Timeout: 5 * time.Second, IdleRefresh: time.Minute, PollInterval: 200 * time.Millisecond, BootstrapDays: 7,
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout")
}

// seenFlags returns the UIDs that carry \Seen.
func seenFlags(t *testing.T, c config.IMAPConfig) []imap.UID {
	t.Helper()
	cl, err := mailbox.Dial(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cl.Close() }()
	if err := cl.Login(c.Username, "secret").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	var all imap.UIDSet
	all.AddRange(1, 0)
	msgs, err := cl.Fetch(all, &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var out []imap.UID
	for _, m := range msgs {
		if slices.Contains(m.Flags, imap.FlagSeen) {
			out = append(out, m.UID)
		}
	}
	return out
}

func runWatcher(t *testing.T, c config.IMAPConfig, sink Sink, poll ...bool) (*Watcher, context.CancelFunc) {
	t.Helper()
	w := New(c, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.forcePoll = len(poll) > 0 && poll[0]
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return w, cancel
}

func testWatch(t *testing.T, caps imap.CapSet, wantMode string) {
	t.Helper()
	srv := start(t, caps)
	now := time.Now()
	srv.add(t, "old1", now.AddDate(0, 0, -30))
	srv.add(t, "old2", now.AddDate(0, 0, -20))
	srv.add(t, "recent", now.AddDate(0, 0, -1))
	c := cfg(t, srv.addr)
	sink := &memSink{}
	w, _ := runWatcher(t, c, sink, wantMode == "poll")

	// Bootstrap: only the recent message, silently; old ones are skipped
	// and never fetched later.
	waitFor(t, func() bool { return sink.cursor().BootstrapDone })
	got := sink.snapshot()
	if len(got) != 1 || got[0].UID != 3 || !got[0].Silent || !bytes.Contains(got[0].Raw, []byte("Corps recent")) {
		t.Fatalf("bootstrap delivered %+v", got)
	}
	if cur := sink.cursor(); cur.LastUID != 3 {
		t.Fatalf("cursor %+v", cur)
	}

	srv.add(t, "new", now)
	waitFor(t, func() bool { return len(sink.snapshot()) == 2 })
	n := sink.snapshot()[1]
	if n.UID != 4 || n.Silent {
		t.Fatalf("new message %+v", n)
	}
	h := w.Health()
	if !h.Connected || h.Mode != wantMode || h.LastOK.IsZero() {
		t.Fatalf("health %+v", h)
	}
	if seen := seenFlags(t, c); len(seen) != 0 {
		t.Fatalf("watcher must never set \\Seen, got %v", seen)
	}
}

func TestWatchIdle(t *testing.T) {
	testWatch(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}}, "idle")
}

func TestWatchPoll(t *testing.T) {
	testWatch(t, imap.CapSet{imap.CapIMAP4rev1: {}}, "poll")
}

func TestResumeFromCursorAndUIDValidityChange(t *testing.T) {
	srv := start(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}})
	now := time.Now()
	srv.add(t, "a", now.Add(-3*time.Hour))
	srv.add(t, "b", now.Add(-2*time.Hour))
	srv.add(t, "c", now.Add(-time.Minute))
	c := cfg(t, srv.addr)

	// Cursor from a previous run: saw UID 1, validity unchanged -> 2 and 3
	// are fetched normally.
	vals := uidValidity(t, c)
	sink := &memSink{cur: Cursor{UIDValidity: vals, LastUID: 1, BootstrapDone: true, LastOK: now.Add(-time.Hour)}}
	runWatcher(t, c, sink)
	waitFor(t, func() bool { return len(sink.snapshot()) == 2 })
	for _, f := range sink.snapshot() {
		if f.Silent || f.UID == 1 {
			t.Fatalf("resume delivered %+v", f)
		}
	}

	// Validity changed: rescan the window; messages older than LastOK are
	// silent, newer ones normal.
	sink2 := &memSink{cur: Cursor{UIDValidity: vals + 1, LastUID: 99, BootstrapDone: true, LastOK: now.Add(-90 * time.Minute)}}
	runWatcher(t, c, sink2)
	waitFor(t, func() bool { return len(sink2.snapshot()) == 3 })
	silent := map[uint32]bool{}
	for _, f := range sink2.snapshot() {
		silent[f.UID] = f.Silent
	}
	if !silent[1] || !silent[2] || silent[3] {
		t.Fatalf("silent map %v", silent)
	}
	waitFor(t, func() bool { return sink2.cursor().UIDValidity == vals })
}

func uidValidity(t *testing.T, c config.IMAPConfig) uint32 {
	cl, err := mailbox.Dial(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cl.Close() }()
	_ = cl.Login(c.Username, "secret").Wait()
	d, err := cl.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return d.UIDValidity
}

func TestAuthFailureReported(t *testing.T) {
	srv := start(t, imap.CapSet{imap.CapIMAP4rev1: {}})
	c := cfg(t, srv.addr)
	c.Password = config.NewSecret("wrong")
	w, _ := runWatcher(t, c, &memSink{})
	waitFor(t, func() bool { return w.Health().AuthFailed })
}
