package tunnel

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testToken has the shape of an enrollment token and is assembled at run time.
var testToken = "nxt_" + strings.Repeat("A", 8) + "BCDEFG"

// fakeApp is the gateway's agent-facing routes.
type fakeApp struct {
	t       *testing.T
	srv     *httptest.Server
	gw      *testGateway
	mu      sync.Mutex
	enrolls []enrollRequest
	rotated []map[string]any
	status  int
	key     AgentKey
	hints   []string
}

func newFakeApp(t *testing.T) *fakeApp {
	t.Helper()
	app := &fakeApp{t: t, gw: newTestGateway(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/networks/enroll", func(w http.ResponseWriter, r *http.Request) {
		var req enrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		app.mu.Lock()
		app.enrolls = append(app.enrolls, req)
		st := app.status
		app.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"error":"token_used","message":"this token was already used"}`))
			return
		}
		proof := req.Proof
		req.Proof = nil
		raw, _ := json.Marshal(req)
		sum := sha256.Sum256([]byte(req.Token))
		if !ed25519.Verify(ed25519.PublicKey(req.SignPub), digestOf("ctun.v1 enroll", raw, sum[:]), proof) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"proof_invalid"}`))
			return
		}
		jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": app.gw.kid, "x": b64url(app.gw.keys[app.gw.kid])}}})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_id": "nag_9", "network_id": "net_1", "org_id": "org_1", "org_slug": "acme", "broker_url": "https://broker.example",
			"gateway_jwks": json.RawMessage(jwks), "gateway_kids": []string{app.gw.kid}, "key_not_after": time.Now().Add(10 * 24 * time.Hour).UTC(),
		})
	})
	mux.HandleFunc("/api/networks/agents/rotate/challenge", func(w http.ResponseWriter, r *http.Request) {
		if !app.verifyJWT(r.Header.Get("Authorization")) {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"nonce":"` + base64.StdEncoding.EncodeToString([]byte("server-nonce-123")) + `"}`))
	})
	mux.HandleFunc("/api/networks/agents/rotate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AgentID   string `json:"agent_id"`
			NewSign   []byte `json:"new_sign_pub"`
			NewEnc    []byte `json:"new_enc_pub"`
			Nonce     []byte `json:"nonce"`
			Signature []byte `json:"signature"`
			OldKID    string `json:"old_kid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		app.mu.Lock()
		key := app.key
		app.mu.Unlock()
		if !app.verifyJWT(r.Header.Get("Authorization")) || !ed25519.Verify(key.SignPub(), digestOf("ctun.v1 rotate", body.Nonce, body.NewSign, body.NewEnc), body.Signature) {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"signature_invalid"}`))
			return
		}
		app.mu.Lock()
		app.rotated = append(app.rotated, map[string]any{"agent_id": body.AgentID, "old_kid": body.OldKID})
		app.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"key_not_after": time.Now().Add(30 * 24 * time.Hour).UTC()})
	})
	mux.HandleFunc(GatewayKeysPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k-new","x":"` + b64url(app.gw.keys[app.gw.kid]) + `"}]}`))
	})
	mux.HandleFunc("/api/networks/agents/config-hints", func(w http.ResponseWriter, r *http.Request) {
		if !app.verifyJWT(r.Header.Get("Authorization")) {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"targets": app.hints})
	})
	app.srv = httptest.NewServer(mux)
	t.Cleanup(app.srv.Close)
	return app
}

func (a *fakeApp) verifyJWT(h string) bool {
	parts := strings.Split(strings.TrimPrefix(h, "Bearer "), ".")
	if len(parts) != 3 {
		return false
	}
	a.mu.Lock()
	key := a.key
	a.mu.Unlock()
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key.SignPub(), []byte(parts[0]+"."+parts[1]), sig) {
		return false
	}
	var claims struct {
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
		Net string `json:"net"`
		Jti string `json:"jti"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	return json.Unmarshal(raw, &claims) == nil && claims.Exp > time.Now().Unix() && claims.Iss != "" && claims.Net != "" && claims.Jti != ""
}

func TestEnrollGeneratesKeysRedeemsTheTokenAndWritesAnIdentity(t *testing.T) {
	app := newFakeApp(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "etc", "config.yaml")
	id, err := Enroll(t.Context(), EnrollOptions{AppURL: app.srv.URL, Token: testToken, Name: "erp-dc1-a", StateDir: filepath.Join(dir, "state"), ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	if id.AgentID != "nag_9" || id.NetworkID != "net_1" || id.BrokerURL != "https://broker.example" || id.Name != "erp-dc1-a" || len(id.Keys) != 1 {
		t.Errorf("identity = %+v", id)
	}
	if len(app.enrolls) != 1 || app.enrolls[0].Name != "erp-dc1-a" || app.enrolls[0].Proto[0] != 1 || len(app.enrolls[0].SignPub) != 32 || len(app.enrolls[0].EncPub) != 32 {
		t.Errorf("enroll request = %+v", app.enrolls)
	}
	// The private keys never left: the request holds public keys only.
	rawReq, _ := json.Marshal(app.enrolls[0])
	if strings.Contains(string(rawReq), b64url(id.Keys[0].SignPriv.Seed())) {
		t.Error("the private key was sent")
	}
	path := filepath.Join(dir, "state", IdentityFile)
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("identity file: %v %v", info, err)
	}
	loaded, err := LoadIdentity(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AgentID != id.AgentID || loaded.Current().KID() != id.Current().KID() || string(loaded.Current().EncPriv) != string(id.Current().EncPriv) {
		t.Error("the identity did not survive a round trip")
	}
	if !loaded.Current().NotAfter.Before(time.Now().Add(11*24*time.Hour)) || loaded.KeyValidity < 9*24*time.Hour {
		t.Errorf("key lifetime = %s until %s", loaded.KeyValidity, loaded.Current().NotAfter)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil || cfg.Network != "net_1" {
		t.Errorf("starter config: %v %v", cfg, err)
	}
	if _, err := Enroll(t.Context(), EnrollOptions{AppURL: app.srv.URL, Token: testToken, StateDir: filepath.Join(dir, "second"), ConfigPath: cfgPath}); err != nil {
		t.Fatal(err)
	}
	if raw := readFileT(t, cfgPath); !strings.Contains(raw, "name: erp-dc1-a") {
		t.Error("an existing config was overwritten")
	}
}

func TestEnrollRefusals(t *testing.T) {
	app := newFakeApp(t)
	dir := t.TempDir()
	good := EnrollOptions{AppURL: app.srv.URL, Token: testToken, StateDir: dir}
	for name, mod := range map[string]func(*EnrollOptions){
		"no prefix":  func(o *EnrollOptions) { o.Token = "abcdefghijklmnop" },
		"short":      func(o *EnrollOptions) { o.Token = "nxt_a" },
		"plain http": func(o *EnrollOptions) { o.AppURL = "http://app.example.com" },
		"not a url":  func(o *EnrollOptions) { o.AppURL = "::" },
		"no host":    func(o *EnrollOptions) { o.AppURL = "https://" },
	} {
		o := good
		mod(&o)
		if _, err := Enroll(t.Context(), o); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	app.status = 409
	_, err := Enroll(t.Context(), good)
	var ae *AppError
	if !errors.As(err, &ae) || ae.Status != 409 || ae.Code != "token_used" || !strings.Contains(ae.Error(), "already used") {
		t.Errorf("used token: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, IdentityFile)); serr == nil {
		t.Error("an identity was written for a refused enrollment")
	}
	if (&AppError{Status: 500, Code: "x"}).Error() != "x (HTTP 500)" {
		t.Error("AppError text")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"agent_id":"a"}`)) }))
	defer bad.Close()
	good.AppURL = bad.URL
	if _, err := Enroll(t.Context(), good); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("incomplete answer: %v", err)
	}
	nojson := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>`)) }))
	defer nojson.Close()
	good.AppURL = nojson.URL
	if _, err := Enroll(t.Context(), good); err == nil {
		t.Error("an HTML answer was accepted")
	}
}

func TestIdentityFileIsRefusedWhenOthersCanReadIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	dir := t.TempDir()
	k, _ := GenerateKey(time.Now(), time.Hour)
	id := &Identity{AgentID: "a", NetworkID: "n", Keys: []AgentKey{k}, GatewayJWKS: json.RawMessage(`{}`)}
	if err := id.Save(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, IdentityFile)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(dir); err == nil || !strings.Contains(err.Error(), "can be read by others") {
		t.Errorf("world-readable identity: %v", err)
	}
	_ = os.Chmod(p, 0o600)
	for name, content := range map[string]string{"not json": "{", "no keys": `{"version":1,"agent_id":"a","keys":[]}`, "bad pem": `{"version":1,"agent_id":"a","keys":[{"sign_pem":"x","enc_pem":"y"}]}`} {
		_ = os.WriteFile(p, []byte(content), 0o600)
		if _, err := LoadIdentity(dir); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestKeyRotationKeepsTheOldKeyForADay(t *testing.T) {
	app := newFakeApp(t)
	dir := t.TempDir()
	now := time.Now()
	old, _ := GenerateKey(now.Add(-21*24*time.Hour), 30*24*time.Hour)
	app.key = old
	id := &Identity{AgentID: "nag_1", NetworkID: "net_1", AppURL: app.srv.URL, Keys: []AgentKey{old}, KeyValidity: 30 * 24 * time.Hour}
	if !id.NeedsRotation(now) {
		t.Fatal("a key with 9 of 30 days left must rotate")
	}
	fresh, _ := GenerateKey(now, 30*24*time.Hour)
	if (&Identity{Keys: []AgentKey{fresh}}).NeedsRotation(now) {
		t.Error("a fresh key rotates")
	}
	if err := RotateKeys(t.Context(), http.DefaultClient, id, dir, now); err != nil {
		t.Fatal(err)
	}
	if len(id.Keys) != 2 || id.Current().KID() == old.KID() || id.Keys[1].RetiredAt == nil {
		t.Fatalf("keys = %d, current is the old one: %v", len(id.Keys), id.Current().KID() == old.KID())
	}
	if len(app.rotated) != 1 || app.rotated[0]["old_kid"] != old.KID() {
		t.Errorf("rotation request = %v", app.rotated)
	}
	if len(id.ActiveKeys(now.Add(23*time.Hour))) != 2 || len(id.ActiveKeys(now.Add(25*time.Hour))) != 1 {
		t.Error("the old key must open requests for 24 hours and not after")
	}
	saved, err := LoadIdentity(dir)
	if err != nil || len(saved.Keys) != 2 || saved.Keys[1].RetiredAt == nil {
		t.Errorf("saved identity: %v %v", saved, err)
	}
	// A rotation after another one drops keys retired for more than a day.
	app.mu.Lock()
	app.key = id.Current()
	app.mu.Unlock()
	if err := RotateKeys(t.Context(), http.DefaultClient, id, dir, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(id.Keys) != 2 {
		t.Errorf("keys after the second rotation = %d", len(id.Keys))
	}
	// A refusal leaves the identity alone.
	broken := &Identity{AgentID: "nag_1", NetworkID: "net_1", AppURL: app.srv.URL, Keys: []AgentKey{fresh}}
	if err := RotateKeys(t.Context(), http.DefaultClient, broken, dir, now); err == nil || len(broken.Keys) != 1 {
		t.Errorf("an unauthorised rotation: %v", err)
	}
	if err := RotateKeys(t.Context(), http.DefaultClient, &Identity{AppURL: "http://x.example"}, dir, now); err == nil {
		t.Error("rotation over plain http")
	}
}

func TestRefreshGatewayKeysAndConfigHints(t *testing.T) {
	app := newFakeApp(t)
	app.hints = []string{"erp", "crm"}
	dir := t.TempDir()
	k, _ := GenerateKey(time.Now(), time.Hour)
	app.key = k
	id := &Identity{AgentID: "nag_1", NetworkID: "net_1", AppURL: app.srv.URL, Keys: []AgentKey{k}, GatewayJWKS: json.RawMessage(`{}`)}
	if err := RefreshGatewayKeys(t.Context(), http.DefaultClient, id, dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	keys, err := id.GatewayKeys()
	if err != nil || keys["k-new"] == nil {
		t.Errorf("keys = %v %v", keys, err)
	}
	hints, err := ConfigHints(t.Context(), http.DefaultClient, id, time.Now())
	if err != nil || strings.Join(hints, ",") != "erp,crm" {
		t.Errorf("hints = %v %v", hints, err)
	}
	other, _ := GenerateKey(time.Now(), time.Hour)
	id.Keys = []AgentKey{other}
	if _, err := ConfigHints(t.Context(), http.DefaultClient, id, time.Now()); err == nil {
		t.Error("hints for an agent the gateway does not know")
	}
}

func TestMintJWTShape(t *testing.T) {
	k, _ := GenerateKey(time.Now(), time.Hour)
	tok, err := MintJWT(k, "nag_1", "https://broker.example", "net_1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	var hdr map[string]string
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(raw, &hdr)
	var claims map[string]any
	raw, _ = base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &claims)
	if len(parts) != 3 || hdr["alg"] != "EdDSA" || hdr["kid"] != k.KID() || claims["iss"] != "nag_1" || claims["aud"] != "https://broker.example" || claims["net"] != "net_1" || claims["jti"] == "" {
		t.Errorf("token = %v %v", hdr, claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat != 60 {
		t.Errorf("lifetime = %v s", exp-iat)
	}
	tok2, _ := MintJWT(k, "nag_1", "https://broker.example", "net_1", time.Now())
	if tok == tok2 {
		t.Error("two tokens are identical: the jti must differ")
	}
}
