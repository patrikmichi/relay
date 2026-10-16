package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Finding is one result of `check`.
type Finding struct {
	Target string `json:"target"`
	Level  string `json:"level"` // error or warn
	Msg    string `json:"msg"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s: %s", f.Level, f.Target, f.Msg)
}

// CheckTargets validates what can be validated without contacting the
// gateway: names resolve, resolved addresses are ones the target may reach,
// TLS handshakes complete, commands exist and secret references resolve.
func CheckTargets(ctx context.Context, cfg *Config, res Resolver) []Finding {
	var out []Finding
	add := func(t Target, level, format string, a ...any) {
		out = append(out, Finding{Target: t.Name, Level: level, Msg: fmt.Sprintf(format, a...)})
	}
	for _, t := range cfg.Targets {
		if t.TLS.InsecureSkipVerify {
			add(t, "warn", "tls.insecure_skip_verify is on: the target's certificate is not checked")
		}
		if t.Credentials != nil {
			for _, ref := range []string{t.Credentials.Value, t.Credentials.Bearer} {
				if ref == "" {
					continue
				}
				if _, err := ResolveSecret(ref); err != nil {
					add(t, "error", "credentials: %v", err)
				}
			}
			if b := t.Credentials.Basic; b != nil {
				for _, ref := range []string{b.Username, b.Password} {
					if _, err := ResolveSecret(ref); err != nil {
						add(t, "error", "credentials: %v", err)
					}
				}
			}
		}
		for k, v := range t.Env {
			if _, err := ResolveSecret(v); err != nil {
				add(t, "error", "env %s: %v", k, err)
			}
		}
		if t.Kind == KindMCPStdio {
			if _, err := exec.LookPath(t.Command); err != nil {
				add(t, "error", "command %q was not found: %v", t.Command, err)
			}
			if t.Cwd != "" {
				if info, err := os.Stat(t.Cwd); err != nil || !info.IsDir() {
					add(t, "error", "cwd %q is not a directory", t.Cwd)
				}
			}
			continue
		}
		out = append(out, checkHTTPTarget(ctx, t, res)...)
	}
	return out
}

func checkHTTPTarget(ctx context.Context, t Target, res Resolver) []Finding {
	var out []Finding
	add := func(level, format string, a ...any) {
		out = append(out, Finding{Target: t.Name, Level: level, Msg: fmt.Sprintf(format, a...)})
	}
	u, err := url.Parse(t.URL)
	if err != nil {
		add("error", "url: %v", err)
		return out
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = append(ips, ip)
	} else {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, lerr := res.LookupIPAddr(rctx, host)
		cancel()
		if lerr != nil || len(addrs) == 0 {
			add("error", "%s does not resolve", host)
			return out
		}
		for _, a := range addrs {
			if ip, ok := netip.AddrFromSlice(a.IP); ok {
				ips = append(ips, ip)
			}
		}
	}
	for _, ip := range ips {
		if blocked, why := ipBlocked(ip, t); blocked {
			add("error", "%v", why)
		}
	}
	if u.Scheme != "https" {
		add("warn", "the target is reached without TLS")
		return out
	}
	if len(out) > 0 && out[len(out)-1].Level == "error" {
		return out
	}
	cfg := &tls.Config{ServerName: t.TLS.ServerName, MinVersion: tls.VersionTLS12}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	if t.TLS.InsecureSkipVerify && t.AllowInsecure {
		cfg.InsecureSkipVerify = true //nolint:gosec // opted in by the operator
	}
	if t.TLS.CAFile != "" {
		pem, rerr := os.ReadFile(t.TLS.CAFile)
		if rerr != nil {
			add("error", "tls.ca_file: %v", rerr)
			return out
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			add("error", "tls.ca_file holds no certificate")
			return out
		}
		cfg.RootCAs = pool
	}
	if t.TLS.ClientCert != "" {
		cert, cerr := tls.LoadX509KeyPair(t.TLS.ClientCert, t.TLS.ClientKey)
		if cerr != nil {
			add("error", "tls client certificate: %v", cerr)
			return out
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: cfg}
	conn, derr := d.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].String(), port))
	if derr != nil {
		add("error", "TLS handshake with %s failed: %v", net.JoinHostPort(host, port), derr)
		return out
	}
	_ = conn.Close()
	return out
}

// Redacted is a copy of the config for printing: literal secret values are
// hidden, references (${file:...}) are kept.
func (c *Config) Redacted() *Config {
	cp := *c
	cp.Targets = append([]Target(nil), c.Targets...)
	hide := func(s string) string {
		if s == "" || strings.Contains(s, "${") {
			return s
		}
		return "<literal hidden>"
	}
	for i := range cp.Targets {
		t := &cp.Targets[i]
		if t.Credentials != nil {
			cr := *t.Credentials
			cr.Value, cr.Bearer = hide(cr.Value), hide(cr.Bearer)
			if cr.Basic != nil {
				b := *cr.Basic
				b.Username, b.Password = hide(b.Username), hide(b.Password)
				cr.Basic = &b
			}
			t.Credentials = &cr
		}
		if len(t.Env) > 0 {
			env := map[string]string{}
			for k, v := range t.Env {
				env[k] = hide(v)
			}
			t.Env = env
		}
	}
	return &cp
}

// PrintConfig renders the redacted config as YAML.
func (c *Config) PrintConfig() ([]byte, error) {
	b, err := yaml.Marshal(c.Redacted())
	if err != nil {
		return nil, err
	}
	return b, nil
}

// UnservedHints lists target names the gateway's bindings use that no target
// of this config serves.
func UnservedHints(cfg *Config, hints []string) []string {
	var out []string
	for _, h := range hints {
		if _, ok := cfg.Target(h); !ok {
			out = append(out, h)
		}
	}
	return out
}

// ErrFindings is returned by callers that turn error findings into a failure.
var ErrFindings = errors.New("the check found problems")

// HasErrors reports whether any finding is an error.
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Level == "error" {
			return true
		}
	}
	return false
}

// CheckAddress reports why an address is not reachable for a target.
func CheckAddress(ip netip.Addr, t Target) error {
	if blocked, why := ipBlocked(ip, t); blocked {
		return why
	}
	return nil
}
