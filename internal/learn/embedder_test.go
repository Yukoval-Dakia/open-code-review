package learn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBigModelEmbedderEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("Authorization = %q, want Bearer tok123", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["model"] != "embedding-3" {
			t.Errorf("model = %v, want embedding-3", req["model"])
		}
		if req["input"] != "hello" {
			t.Errorf("input = %v, want hello", req["input"])
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3]}],"model":"embedding-3"}`)
	}))
	defer srv.Close()

	e := NewBigModelEmbedder(srv.URL, "tok123", "embedding-3")
	got, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 3 || got[0] != 0.1 || got[2] != 0.3 {
		t.Fatalf("embedding = %v, want [0.1 0.2 0.3]", got)
	}
}

func TestBigModelEmbedderHTTPErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()
	e := NewBigModelEmbedder(srv.URL, "t", "embedding-3")
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 500")
	} else if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention status: %v", err)
	}
}
