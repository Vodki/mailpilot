// Package tgtest is a fake Bot API server for tests.
package tgtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Call is one recorded API call.
type Call struct {
	Method string
	Params map[string]any
}

// Server is a fake Telegram API.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []Call
	updates []map[string]any
	nextID  int64
	notify  chan struct{}
	// Fail makes the given method return an error.
	Fail map[string]bool
}

// New starts a fake server; the token is ignored.
func New(t *testing.T) *Server {
	s := &Server{nextID: 100, notify: make(chan struct{}, 1), Fail: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// Push queues an update for getUpdates.
func (s *Server) Push(u map[string]any) {
	s.mu.Lock()
	u["update_id"] = int64(len(s.updates) + 1)
	s.updates = append(s.updates, u)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Calls returns recorded calls of a method ("" for all).
func (s *Server) Calls(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Texts returns the text of every sendMessage, in order.
func (s *Server) Texts() []string {
	var out []string
	for _, c := range s.Calls("sendMessage") {
		t, _ := c.Params["text"].(string)
		out = append(out, t)
	}
	return out
}

// WaitFor polls until f is true or the timeout expires.
func WaitFor(t *testing.T, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	method := parts[len(parts)-1]
	body, _ := io.ReadAll(r.Body)
	params := map[string]any{}
	_ = json.Unmarshal(body, &params)

	if method == "getUpdates" {
		s.getUpdates(w, r, params)
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Params: params})
	fail := s.Fail[method]
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	if fail {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"Internal"}`))
		return
	}
	var result any = true
	switch method {
	case "sendMessage":
		chat, _ := params["chat_id"].(float64)
		result = map[string]any{"message_id": id, "chat": map[string]any{"id": int64(chat), "type": "private"}, "text": params["text"]}
	case "getMe":
		result = map[string]any{"id": 1, "is_bot": true, "first_name": "bot", "username": "bot"}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request, params map[string]any) {
	offset, _ := params["offset"].(float64)
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		var out []map[string]any
		for _, u := range s.updates {
			if float64(u["update_id"].(int64)) >= offset {
				out = append(out, u)
			}
		}
		s.mu.Unlock()
		if len(out) > 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
			return
		}
		select {
		case <-s.notify:
		case <-deadline:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// PrivateMessage builds a message update from chat/user id.
func PrivateMessage(chatID, fromID int64, text string) map[string]any {
	return map[string]any{"message": map[string]any{
		"message_id": 1, "date": 0, "text": text,
		"chat": map[string]any{"id": chatID, "type": "private"},
		"from": map[string]any{"id": fromID, "is_bot": false, "first_name": "x"},
	}}
}

// CallbackPress builds a callback_query update.
func CallbackPress(chatID, fromID int64, data string) map[string]any {
	return map[string]any{"callback_query": map[string]any{
		"id": "cb1", "data": data,
		"from":    map[string]any{"id": fromID, "is_bot": false, "first_name": "x"},
		"message": map[string]any{"message_id": 7, "date": 0, "chat": map[string]any{"id": chatID, "type": "private"}},
	}}
}
