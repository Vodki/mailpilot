package todoist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Vodki/mailpilot/internal/config"
)

func TestCurrentUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" || r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":"123","full_name":"Louis","email":"x@y.z"}`))
	}))
	defer srv.Close()

	u, err := New(srv.URL+"/api/v1", config.NewSecret("good"), srv.Client()).CurrentUser(context.Background())
	if err != nil || u.ID != "123" {
		t.Fatalf("user %+v err %v", u, err)
	}
	if _, err := New(srv.URL+"/api/v1", config.NewSecret("bad"), srv.Client()).CurrentUser(context.Background()); err == nil {
		t.Fatal("expected 401 error")
	}
	if _, err := New(srv.URL, config.Secret{}, nil).CurrentUser(context.Background()); err == nil {
		t.Fatal("expected missing token error")
	}
}

func TestCreateTask(t *testing.T) {
	var got map[string]any
	var reqID, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reqID, auth = r.Header.Get("X-Request-Id"), r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"6X7rM8997g3RQmvh","content":"x"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, config.NewSecret("tok"), srv.Client())
	task, err := c.CreateTask(context.Background(), NewTask{Content: "Valider le devis", DueDate: "2026-11-27", Priority: 3, Labels: []string{"mailpilot"}}, "mailpilot-1-0")
	if err != nil || task.ID != "6X7rM8997g3RQmvh" {
		t.Fatalf("create: %v %+v", err, task)
	}
	if reqID != "mailpilot-1-0" || auth != "Bearer tok" || got["due_date"] != "2026-11-27" || got["content"] != "Valider le devis" {
		t.Fatalf("request %v %q %q", got, reqID, auth)
	}
	if _, ok := got["due_datetime"]; ok {
		t.Fatal("empty fields must be omitted")
	}
}
