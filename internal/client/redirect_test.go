package client

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetRedirect_ReturnsTheTargetWithoutFollowingIt(t *testing.T) {
	var targetAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = r.Header.Get("Authorization")
	}))
	defer target.Close()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/fil_1/download":
			http.Redirect(w, r, target.URL+"/blob?sig=abc", http.StatusFound)
		case "/api/direct":
			_, _ = io.WriteString(w, "bytes")
		case "/api/relative":
			http.Redirect(w, r, "/api/direct", http.StatusFound)
		}
	}))
	defer gw.Close()
	c := New(gw.URL, "secret")

	resp, loc, err := c.GetRedirect(t.Context(), "/api/files/fil_1/download")
	if err != nil || resp != nil || loc != target.URL+"/blob?sig=abc" {
		t.Fatalf("resp %v, loc %q, err %v", resp, loc, err)
	}
	if targetAuth != "" {
		t.Errorf("the target saw credentials %q", targetAuth)
	}

	resp, loc, err = c.GetRedirect(t.Context(), "/api/direct")
	if err != nil || loc != "" || resp == nil {
		t.Fatalf("resp %v, loc %q, err %v", resp, loc, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "bytes" {
		t.Errorf("body = %q", body)
	}

	if _, loc, err = c.GetRedirect(t.Context(), "/api/relative"); err != nil || loc != gw.URL+"/api/direct" {
		t.Errorf("relative loc %q, err %v", loc, err)
	}
}
