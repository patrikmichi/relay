package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayMessageStripsControlCharacters(t *testing.T) {
	got := gatewayMessage([]byte(`{"error":"Forbidden: \u001b[2J\u001b[1;1Hpay\u009battacker\r\nnow"}`))
	if got != "[2J[1;1Hpayattackernow" {
		t.Fatalf("gatewayMessage = %q", got)
	}
}

func TestResolveFieldValue(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	used := false
	if v, lit, err := resolveFieldValue("plain", nil, &used); v != "plain" || !lit || err != nil {
		t.Errorf("literal = %q %v %v", v, lit, err)
	}
	if v, lit, err := resolveFieldValue("@"+file, nil, &used); v != "from-file" || lit || err != nil {
		t.Errorf("@file = %q %v %v", v, lit, err)
	}
	if v, lit, err := resolveFieldValue("-", strings.NewReader("from-stdin\r\n"), &used); v != "from-stdin" || lit || err != nil {
		t.Errorf("stdin = %q %v %v", v, lit, err)
	}
	if _, _, err := resolveFieldValue("-", strings.NewReader("again"), &used); err == nil {
		t.Error("stdin read twice")
	}
	if _, _, err := resolveFieldValue("@"+file+".missing", nil, &used); err == nil {
		t.Error("missing file accepted")
	}
	big := strings.Repeat("x", maxFieldBytes+1)
	if _, _, err := resolveFieldValue("-", strings.NewReader(big), new(bool)); err == nil {
		t.Error("oversized stdin accepted")
	}
}

func TestConnectWarnsThatArgvIsVisibleOnlyForLiteralSecrets(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newConnectorGateway(t)

	out, err := runWithInput(t, ConnectCmd(), "", "clockify", "--field", "CLOCKIFY_API_KEY=literal-secret", "--reconnect")
	if err != nil || !strings.Contains(out, "visible to other users in `ps`") {
		t.Fatalf("literal secret: %v\n%s", err, out)
	}
	out, err = runWithInput(t, ConnectCmd(), "", "clockify", "--field", "CLOCKIFY_API_KEY=@"+file, "--reconnect")
	if err != nil || strings.Contains(out, "visible") {
		t.Fatalf("file secret: %v\n%s", err, out)
	}
	if out, err = runWithInput(t, ConnectCmd(), "piped\n", "clockify", "--field", "CLOCKIFY_API_KEY=-", "--reconnect"); err != nil {
		t.Fatalf("stdin secret: %v\n%s", err, out)
	}
	var got []string
	for _, s := range g.saved {
		got = append(got, s["fields"].(map[string]any)["CLOCKIFY_API_KEY"].(string))
	}
	if strings.Join(got, ",") != "literal-secret,s3cret,piped" {
		t.Errorf("gateway received %v", got)
	}
}
