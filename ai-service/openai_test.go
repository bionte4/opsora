package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICheckAndChat(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing bearer token")
		}
		sawAuth = true
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"llama-3.1-8b-instant"}]}`))
		case "/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newOpenAI(srv.URL, "test-key", "llama-3.1-8b-instant", "")
	info, err := p.Check(t.Context())
	if err != nil || !info.ModelAvailable {
		t.Fatalf("check: %+v %v", info, err)
	}
	reply, err := p.Chat(t.Context(), "system", "ping", nil)
	if err != nil || reply != "OK" {
		t.Fatalf("chat: %q %v", reply, err)
	}
	if _, err := p.Embed(t.Context(), "x"); err != errEmbedDisabled {
		t.Fatalf("embed without a model: %v", err)
	}
	if !sawAuth || strings.Contains(p.Endpoint(), "test-key") {
		t.Fatal("endpoint must not contain the API key")
	}
}
