package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Vodki/mailpilot/internal/config"
)

func TestGetMe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN123/getMe" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"Pilot","username":"mailpilot_bot"}}`))
	}))
	defer srv.Close()

	u, err := New(srv.URL, config.NewSecret("TOKEN123"), srv.Client()).GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "mailpilot_bot" || !u.IsBot {
		t.Fatalf("unexpected user %+v", u)
	}

	_, err = New(srv.URL, config.NewSecret("WRONG"), srv.Client()).GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "WRONG") {
		t.Fatalf("expected API error without token, got %v", err)
	}
}

func TestTokenNeverInTransportError(t *testing.T) {
	c := New("http://127.0.0.1:1", config.NewSecret("SECRET-TOKEN"), nil)
	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected connection error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestMissingToken(t *testing.T) {
	if _, err := New("http://x", config.Secret{}, nil).GetMe(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
