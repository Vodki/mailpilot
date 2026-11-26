package todoist

import (
	"context"
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
