package redact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrikmichi/zuano/internal/transcripts"
)

// Secret-shaped test values are built at run time so that no literal in the
// repository looks like a real credential.
func join(parts ...string) string { return strings.Join(parts, "") }

func rep(c string, n int) string { return strings.Repeat(c, n) }

func TestSecretsAreRemoved(t *testing.T) {
	jwt := join("eyJ", rep("a", 10), ".eyJ", rep("b", 10), ".", rep("c", 12))
	pem := join("-----BEGIN RSA PRIVATE ", "KEY-----\nMIIabc\n-----END RSA PRIVATE ", "KEY-----")
	for name, secret := range map[string]string{
		"model provider": join("sk", "-", rep("a", 24)),
		"payment live":   join("sk", "_live_", rep("A", 20)),
		"payment rk":     join("rk", "_live_", rep("A", 20)),
		"payment test":   join("sk", "_test_", rep("1", 20)),
		"webhook":        join("whsec", "_", rep("Z", 20)),
		"source host":    join("ghp", "_", rep("x", 36)),
		"fine grained":   join("github", "_pat_", rep("y", 50)),
		"chat":           join("xoxb", "-", rep("1", 12), "-", rep("a", 12)),
		"cloud id":       join("AKIA", rep("A", 16)),
		"cloud api":      join("AIza", rep("b", 35)),
		"jwt":            jwt,
		"private key":    pem,
		"bearer":         "Bearer " + rep("t", 30),
		"gateway key":    join("mcp", "_", rep("a", 64)),
	} {
		got := New(nil).Text("before " + secret + " after")
		if strings.Contains(got, secret) || !strings.Contains(got, "⟦SECRET_1⟧") || !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after") {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func TestAssignmentsAndEntropyNearSecretKeys(t *testing.T) {
	r := New(nil)
	for in, keep := range map[string]string{
		"DB_PASSWORD=hunter2hunter2":                         "DB_PASSWORD=",
		`{"api_key": "` + "abcd" + `1234efgh"}`:              `{"api_key": "`,
		"export API_KEY='zzzzzzzzzz'":                        "export API_KEY='",
		"password: correct-horse-battery":                    "password: ",
		"client_secret = " + rep("q", 12):                    "client_secret = ",
		"the token is " + "Zx9Qm2Lp8Vb4Nc7Rt1Ys5Wd3Hf6Jk0Aa": "the token is ",
	} {
		got := r.Text(in)
		if !strings.HasPrefix(got, keep) || !strings.Contains(got, "⟦SECRET_") {
			t.Errorf("%q -> %q", in, got)
		}
	}
	// Not secrets: no secret-like word, or too little entropy.
	for _, in := range []string{
		"commit " + rep("a", 40),
		"id 3f2b8c1e-9d4a-4e7b-8a21-5c6d7e8f9a0b",
		"the token is " + rep("a", 30),
		"password",
		"tokens are counted per request",
	} {
		if got := New(nil).Text(in); got != in {
			t.Errorf("changed %q -> %q", in, got)
		}
	}
}

func TestURLsKeepTheirHost(t *testing.T) {
	r := New(nil)
	got := r.Text("clone https://alice:s3cretpass@git.example.com/repo.git and fetch https://api.example.com/x?token=abc123&page=2")
	if strings.Contains(got, "alice") || strings.Contains(got, "s3cretpass") || strings.Contains(got, "abc123") ||
		!strings.Contains(got, "git.example.com") || !strings.Contains(got, "api.example.com") || !strings.Contains(got, "page=2") {
		t.Errorf("got %q", got)
	}
}

func TestPersonalDataClasses(t *testing.T) {
	cases := map[string]string{
		"mail ann@example.com now":           "⟦EMAIL_1⟧",
		"call +421 905 123 456":              "⟦PHONE_1⟧",
		"call (415) 555-2671":                "⟦PHONE_1⟧",
		"call 415-555-2671":                  "⟦PHONE_1⟧",
		"ssn 123-45-6789":                    "⟦ID_1⟧",
		"rc 900101/1234":                     "⟦ID_1⟧",
		"card 4111 1111 1111 1111":           "⟦CARD_1⟧",
		"iban SK3112000000198742637541":      "⟦IBAN_1⟧",
		"server 8.8.8.8 down":                "⟦IP_1⟧",
		"v6 2001:db8:85a3:0:0:8a2e:370:7334": "⟦IP_1⟧",
	}
	for in, want := range cases {
		if got := New(nil).Text(in); !strings.Contains(got, want) {
			t.Errorf("%q -> %q, want %s", in, got, want)
		}
	}
	for _, in := range []string{
		"card 4111 1111 1111 1112", "iban SK3112000000198742637542", "rc 901301/1234",
		"host 10.0.0.1 and 192.168.1.5 and 127.0.0.1", "build 1234567890", "uuid 3f2b8c1e-9d4a-4e7b-8a21-5c6d7e8f9a0b",
		"sha " + rep("0123456789abcdef", 2), "version 1.2.3", "ip ::1", "n 999.1.1.1",
	} {
		if got := New(nil).Text(in); got != in {
			t.Errorf("changed %q -> %q", in, got)
		}
	}
}

func TestPlaceholdersAreConsistentAcrossTexts(t *testing.T) {
	r := New(nil)
	a := r.Text("ann@example.com wrote to bob@example.com")
	b := r.Text("reply to Ann@Example.com")
	if a != "⟦EMAIL_1⟧ wrote to ⟦EMAIL_2⟧" || b != "reply to ⟦EMAIL_1⟧" {
		t.Errorf("%q / %q", a, b)
	}
}

func TestHomeDirectoriesAndCustomTerms(t *testing.T) {
	r := New([]string{"  Acme Corp ", "", "acme"})
	got := r.Text(`open /home/ann/code/x and /Users/bob/y and C:\Users\carl\z for ACME CORP and acme`)
	for _, gone := range []string{"ann", "bob", "carl", "ACME", "acme"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q still in %q", gone, got)
		}
	}
	if !strings.Contains(got, "~/code/x") || !strings.Contains(got, "⟦TERM_1⟧") {
		t.Errorf("got %q", got)
	}
	if RulesVersion() != "1" || len(RulesJSON()) == 0 {
		t.Error("rules not embedded")
	}
}

func TestSessionRedactsEverythingAndReports(t *testing.T) {
	key := join("sk", "-", rep("k", 24))
	s := &transcripts.Session{Format: transcripts.Format, Title: "mail ann@example.com", Turns: []transcripts.Turn{
		{I: 0, Role: "user", Text: "my key is " + key + " and ann@example.com"},
		{I: 1, Role: "assistant", Text: "ok", ToolCalls: []transcripts.ToolCall{
			{Name: "http", Kind: "builtin", ArgsPreview: `{"url":"https://x.example/?token=zzz","password":"harmless","nested":{"api_key":12345,"list":["bob@example.com",1]}}`, ResultPreview: "sent to bob@example.com"},
			{Name: "cut", Kind: "builtin", ArgsPreview: `{"password": "abcdefgh…[truncated]`},
		}},
	}}
	Session(s, []string{"example.com"})
	raw, _ := json.Marshal(s)
	for _, leak := range []string{key, "example.com", "zzz", "harmless", "12345", "abcdefgh"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%q leaked: %s", leak, raw)
		}
	}
	rd := s.Redaction
	if !rd.ClientRedacted || rd.RulesVersion != "1" || rd.Placeholders == 0 || rd.Counts[ClassSecret] < 3 || rd.Counts[ClassTerm] == 0 {
		t.Errorf("report = %+v", rd)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(s.Turns[1].ToolCalls[0].ArgsPreview), &args); err != nil || args["password"] == "harmless" {
		t.Errorf("args = %v, %v", args, err)
	}
}

// The gateway applies the same rule file again to what it receives; when its
// fixtures are provided, the two copies must match byte for byte.
func TestRulesMatchTheGatewaysCopy(t *testing.T) {
	dir := os.Getenv("ZUANO_CONTRACT_FIXTURES")
	if dir == "" {
		t.Skip("no gateway fixtures")
	}
	theirs, err := os.ReadFile(filepath.Join(dir, "redaction-rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(theirs) != string(RulesJSON()) {
		t.Error("the embedded redaction rules differ from the gateway's")
	}
}

func TestValidators(t *testing.T) {
	if luhn("1234") || !luhn("4111111111111111") || validIBAN("short") || validSkRc("12") || !validSkRc("900101/1234") {
		t.Error("validators wrong")
	}
	if shannon("aaaa") != 0 || shannon("abcd") != 2 {
		t.Error("entropy wrong")
	}
}
