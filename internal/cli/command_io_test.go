package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/patrikmichi/relay/internal/client"
)

func TestCommandDoerKeepsResponseReadableAndHonorsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "complete response")
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := commandDoer{client.New(srv.URL, "token"), ctx}
	response, err := d.Get("/test")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "complete response" {
		t.Fatalf("response canceled before consumption: %s %v", body, err)
	}
	cancel()
	if _, err := d.Get("/test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
	if err := waitForPublish(d, defaultControlTimeout); !errors.Is(err, context.Canceled) {
		t.Fatalf("poll wait ignored cancellation: %v", err)
	}
}
