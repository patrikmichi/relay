package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallbackValidatesBeforeCompleting(t *testing.T) {
	codes, failures := make(chan string, 1), make(chan error, 1)
	handler := oauthCallbackHandler("expected", codes, failures)
	for _, target := range []string{"/callback?state=wrong&code=secret", "/callback?error=denied", "/callback?state=expected", "/other?state=expected&code=secret"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code < 400 || len(codes) != 0 || len(failures) != 0 {
			t.Fatalf("untrusted callback completed authentication: %s", target)
		}
	}
	for i, want := range []int{http.StatusOK, http.StatusConflict} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/callback?state=expected&code=valid", nil))
		if recorder.Code != want {
			t.Fatalf("callback %d: got %d want %d", i, recorder.Code, want)
		}
	}
	if got := <-codes; got != "valid" {
		t.Fatalf("wrong code: %q", got)
	}
}

func TestExchangeCodeRejectsIncompleteSuccess(t *testing.T) {
	for _, body := range []string{`{}`, `{"access_token":"access","email":"member@example.com","expires_in":3600}`, `{"access_token":"access","refresh_token":"refresh","email":"member@example.com","expires_in":-1}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := exchangeCode(srv.URL, "code", "verifier", "http://127.0.0.1/callback")
		srv.Close()
		if err == nil {
			t.Fatalf("accepted incomplete session: %s", body)
		}
	}
}
