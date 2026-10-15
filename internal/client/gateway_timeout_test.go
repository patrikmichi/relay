package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func TestGatewayClientHasNoClientWideTimeout(t *testing.T) {
	if got := New("https://gw.example.com", "tok").httpClient.Timeout; got != 0 {
		t.Fatalf("gateway client Timeout = %s, want 0 so long tool calls are bounded only by their context", got)
	}
	if got := NoRedirectHTTPClient().Timeout; got != tokenEndpointTimeout {
		t.Fatalf("token endpoint client Timeout = %s, want %s", got, tokenEndpointTimeout)
	}
}

func TestPostContext_DeadlineComesFromCaller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.PostContext(ctx, "/slow", "application/json", nil)
	if err != nil {
		t.Fatalf("slow request within the caller's deadline failed: %v", err)
	}
	_ = resp.Body.Close()

	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := c.PostContext(short, "/slow", "application/json", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded from the caller's deadline", err)
	}
}

func TestRequestsCarryVersionHeaders(t *testing.T) {
	orig := clientVersion
	t.Cleanup(func() { clientVersion = orig })
	SetVersion("v9.8.7")

	var ua, rcv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		rcv = r.Header.Get("Relay-Client-Version")
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "tok").GetContext(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if want := "relay/v9.8.7 (" + runtime.GOOS + "/" + runtime.GOARCH + ")"; ua != want {
		t.Errorf("User-Agent = %q, want %q", ua, want)
	}
	if rcv != "v9.8.7" {
		t.Errorf("Relay-Client-Version = %q, want v9.8.7", rcv)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = NoRedirectHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if rcv != "v9.8.7" {
		t.Errorf("token endpoint Relay-Client-Version = %q, want v9.8.7", rcv)
	}
}

func TestSetVersionIgnoresEmpty(t *testing.T) {
	orig := clientVersion
	t.Cleanup(func() { clientVersion = orig })
	SetVersion("v1.2.3")
	SetVersion("")
	if clientVersion != "v1.2.3" {
		t.Fatalf("clientVersion = %q, want v1.2.3", clientVersion)
	}
}
