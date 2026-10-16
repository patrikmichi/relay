package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/patrikmichi/zuano/internal/brand"
)

// DefaultKeyValidity is how long a key lives when the gateway names no date.
const DefaultKeyValidity = 30 * 24 * time.Hour

// GatewayKeysPath is where the gateway publishes the keys it signs requests
// with.
const GatewayKeysPath = "/.well-known/" + brand.Name + "/ctun-gateway-keys"

// EnrollOptions are the inputs of an enrollment.
type EnrollOptions struct {
	// AppURL is the gateway's address; enrollment, rotation and key fetches go
	// there over HTTPS.
	AppURL   string
	Token    string
	Name     string
	StateDir string
	// ConfigPath, when set and absent, receives a starter config for the
	// network.
	ConfigPath string
	HTTP       *http.Client
	Now        func() time.Time
}

// AppError is a refusal by the gateway's agent routes.
type AppError struct {
	Status int
	Code   string
	Msg    string
}

func (e *AppError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s (%s, HTTP %d)", e.Msg, e.Code, e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
}

// secureURL is an https address, or plain http to the local machine.
func secureURL(u *url.URL) bool {
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopbackURL(u))
}

func checkAppURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%q is not a gateway address; give an https:// URL", raw)
	}
	if !secureURL(u) {
		return nil, fmt.Errorf("the gateway address must be https:// (got %s://)", u.Scheme)
	}
	return u, nil
}

func (o EnrollOptions) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func appRequest(ctx context.Context, c *http.Client, method, rawURL string, auth string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &AppError{Status: resp.StatusCode, Code: e.Error, Msg: e.Message}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("the gateway answered %s with something that is not JSON: %w", req.URL.Path, err)
		}
	}
	return nil
}

type enrollRequest struct {
	Token   string `json:"token"`
	SignPub []byte `json:"sign_pub"`
	EncPub  []byte `json:"enc_pub"`
	Name    string `json:"name"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Proto   []int  `json:"proto"`
	Proof   []byte `json:"proof,omitempty"`
}

type enrollResponse struct {
	AgentID      string          `json:"agent_id"`
	NetworkID    string          `json:"network_id"`
	OrgID        string          `json:"org_id"`
	OrgSlug      string          `json:"org_slug"`
	BrokerURL    string          `json:"broker_url"`
	GatewayJWKS  json.RawMessage `json:"gateway_jwks"`
	GatewayKIDs  []string        `json:"gateway_kids"`
	KeyNotAfter  time.Time       `json:"key_not_after"`
	Status       string          `json:"status"`
	NetworkLabel string          `json:"network_name"`
}

// enrollProof binds the request to the key and the token: Ed25519 over the
// request without the proof, then the SHA-256 of the token.
func enrollProof(key AgentKey, req enrollRequest) ([]byte, error) {
	req.Proof = nil
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(req.Token))
	return ed25519.Sign(key.SignPriv, digestOf("ctun.v1 enroll", raw, sum[:])), nil
}

// Enroll generates the agent's keys, redeems the token and writes the
// identity.
func Enroll(ctx context.Context, o EnrollOptions) (*Identity, error) {
	if !strings.HasPrefix(o.Token, "nxt_") || len(o.Token) < 12 {
		return nil, errors.New("the token must be an enrollment token that starts with nxt_")
	}
	app, err := checkAppURL(o.AppURL)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	name := o.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	now := o.Now()
	key, err := GenerateKey(now, DefaultKeyValidity)
	if err != nil {
		return nil, err
	}
	req := enrollRequest{Token: o.Token, SignPub: key.SignPub(), EncPub: key.EncPub(), Name: name, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Proto: []int{ProtoVersion}}
	if req.Proof, err = enrollProof(key, req); err != nil {
		return nil, err
	}
	var res enrollResponse
	if err := appRequest(ctx, o.client(), http.MethodPost, app.String()+"/api/networks/enroll", "", req, &res); err != nil {
		return nil, err
	}
	if res.AgentID == "" || res.NetworkID == "" || res.BrokerURL == "" {
		return nil, errors.New("the gateway's enrollment answer is incomplete")
	}
	if _, err := ParseJWKS(res.GatewayJWKS); err != nil {
		return nil, err
	}
	if !res.KeyNotAfter.IsZero() {
		key.NotAfter = res.KeyNotAfter
	}
	id := &Identity{
		AgentID: res.AgentID, NetworkID: res.NetworkID, OrgID: res.OrgID, OrgSlug: res.OrgSlug, Name: name,
		BrokerURL: res.BrokerURL, AppURL: app.String(), Keys: []AgentKey{key}, GatewayJWKS: res.GatewayJWKS, JWKSFetched: now,
		KeyValidity: key.NotAfter.Sub(key.CreatedAt),
	}
	if err := id.Save(o.StateDir); err != nil {
		return nil, fmt.Errorf("save the identity: %w", err)
	}
	if o.ConfigPath != "" {
		if err := writeStarterConfig(o.ConfigPath, id); err != nil {
			return id, fmt.Errorf("enrolled, but the starter config could not be written: %w", err)
		}
	}
	return id, nil
}

const starterConfig = `# Written by enrollment. Targets are the only things the gateway may reach:
# list each one by name, with the methods and paths it may use. Reload with
# SIGHUP or by saving the file; ` + BinaryName + ` check validates it offline.
version: 1
network: %s
name: %s
targets: []
#  - name: erp
#    kind: http
#    url: https://erp.internal.example:8443/api/v2
#    cidrs: [10.20.0.0/16]
#    methods: [GET, POST]
#    paths: ["/invoices/**"]
#    credentials:
#      header: Authorization
#      value: "Bearer ${file:/run/secrets/erp-token}"
`

func writeStarterConfig(path string, id *Identity) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf(starterConfig, id.NetworkID, id.Name)), 0o644)
}

type rotateChallenge struct {
	Nonce []byte `json:"nonce"`
}

// RotateKeys replaces the agent's keys: it fetches a server nonce and sends the
// new public keys signed with the old identity key. The old key keeps working
// for a day.
func RotateKeys(ctx context.Context, c *http.Client, id *Identity, stateDir string, now time.Time) error {
	app, err := checkAppURL(id.AppURL)
	if err != nil {
		return err
	}
	old := id.Current()
	token, err := MintJWT(old, id.AgentID, app.String(), id.NetworkID, now)
	if err != nil {
		return err
	}
	var ch rotateChallenge
	if err := appRequest(ctx, c, http.MethodGet, app.String()+"/api/networks/agents/rotate/challenge", token, nil, &ch); err != nil {
		return err
	}
	validity := id.KeyValidity
	if validity <= 0 {
		validity = DefaultKeyValidity
	}
	next, err := GenerateKey(now, validity)
	if err != nil {
		return err
	}
	msg := digestOf("ctun.v1 rotate", ch.Nonce, next.SignPub(), next.EncPub())
	var res struct {
		KeyNotAfter time.Time `json:"key_not_after"`
	}
	body := map[string]any{
		"agent_id": id.AgentID, "new_sign_pub": next.SignPub(), "new_enc_pub": next.EncPub(),
		"nonce": ch.Nonce, "old_kid": old.KID(), "signature": ed25519.Sign(old.SignPriv, msg),
	}
	if err := appRequest(ctx, c, http.MethodPost, app.String()+"/api/networks/agents/rotate", token, body, &res); err != nil {
		return err
	}
	if !res.KeyNotAfter.IsZero() {
		next.NotAfter = res.KeyNotAfter
	}
	id.Rotate(next, now)
	return id.Save(stateDir)
}

// RefreshGatewayKeys fetches the gateway's request-signing keys and stores
// them in the identity.
func RefreshGatewayKeys(ctx context.Context, c *http.Client, id *Identity, stateDir string, now time.Time) error {
	app, err := checkAppURL(id.AppURL)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := appRequest(ctx, c, http.MethodGet, app.String()+GatewayKeysPath, "", nil, &raw); err != nil {
		return err
	}
	if _, err := ParseJWKS(raw); err != nil {
		return err
	}
	id.GatewayJWKS, id.JWKSFetched = raw, now
	return id.Save(stateDir)
}

// ConfigHints asks the gateway which target names the network's bindings use,
// so check can warn about ones no target serves.
func ConfigHints(ctx context.Context, c *http.Client, id *Identity, now time.Time) ([]string, error) {
	app, err := checkAppURL(id.AppURL)
	if err != nil {
		return nil, err
	}
	token, err := MintJWT(id.Current(), id.AgentID, app.String(), id.NetworkID, now)
	if err != nil {
		return nil, err
	}
	var res struct {
		Targets []string `json:"targets"`
	}
	if err := appRequest(ctx, c, http.MethodGet, app.String()+"/api/networks/agents/config-hints", token, nil, &res); err != nil {
		return nil, err
	}
	return res.Targets, nil
}
