package tunnel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := ParseConfig([]byte(repoFile(t, "packaging", "config.example.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network == "" || len(cfg.Targets) != 0 {
		t.Errorf("example = %+v", cfg)
	}
	// The commented examples are valid too once uncommented.
	var lines []string
	inTargets := false
	for _, l := range strings.Split(repoFile(t, "packaging", "config.example.yaml"), "\n") {
		switch {
		case strings.HasPrefix(l, "targets:"):
			inTargets = true
			lines = append(lines, "targets:")
		case inTargets && strings.HasPrefix(l, "#"):
			lines = append(lines, strings.TrimPrefix(l, "#"))
		case !inTargets:
			lines = append(lines, l)
		}
	}
	cfg, err = ParseConfig([]byte(strings.Join(lines, "\n")))
	if err != nil || len(cfg.Targets) != 3 {
		t.Errorf("the commented targets: %v %v", cfg, err)
	}
}

func TestSystemdUnitIsHardened(t *testing.T) {
	unit := repoFile(t, "packaging", "systemd", BinaryName+".service")
	for _, want := range []string{
		"User=zuano-connector", "NoNewPrivileges=true", "ProtectSystem=strict", "ReadWritePaths=/var/lib/zuano-connector",
		"PrivateTmp=true", "RestartSec=5", "Restart=always", "RestartPreventExitStatus=2 3", "ExecReload=/bin/kill -HUP $MAINPID",
		"ExecStart=/usr/bin/zuano-connector run",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit lacks %q", want)
		}
	}
}

func TestManifestFromDistAndSigning(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("zuano-connector_linux_amd64", "linux amd")
	write("zuano-connector_linux_arm64", "linux arm")
	write("zuano-connector_windows_amd64.exe", "windows")
	write("zuano-connector-linux-amd64.tar.gz", "an archive, not a bare binary")
	write("SHA256SUMS.txt", "x")
	write("zuano-connector_bad", "x")
	_ = os.Mkdir(filepath.Join(dir, "zuano-connector_dir_x"), 0o755)

	m, err := ManifestFromDist(dir, BinaryName, "v1.4.2", "https://releases.example/connector/1.4.2/", time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.4.2" || m.MinProto != 1 || len(m.Artifacts) != 3 {
		t.Fatalf("manifest = %+v", m)
	}
	if a := m.Artifacts[0]; a.OS != "linux" || a.Arch != "amd64" || a.URL != "https://releases.example/connector/1.4.2/zuano-connector_linux_amd64" || a.Size != 9 || len(a.SHA256) != 64 {
		t.Errorf("first artifact = %+v", a)
	}
	if a := m.Artifacts[2]; a.OS != "windows" || !strings.HasSuffix(a.URL, ".exe") {
		t.Errorf("windows artifact = %+v", a)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw, sig, err := SignManifest(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(raw, sig, []ed25519.PublicKey{pub}); err != nil {
		t.Errorf("the signed manifest does not verify: %v", err)
	}
	raw[5] ^= 1
	if err := VerifyManifest(raw, sig, []ed25519.PublicKey{pub}); err == nil {
		t.Error("an altered manifest verified")
	}
	if _, err := ManifestFromDist(t.TempDir(), BinaryName, "1.0.0", "https://x", time.Now()); err == nil {
		t.Error("an empty dist directory")
	}
	if _, err := ManifestFromDist(filepath.Join(dir, "none"), BinaryName, "1.0.0", "https://x", time.Now()); err == nil {
		t.Error("a missing dist directory")
	}
}

func helm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	var out bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}

type k8sDoc = map[string]any

func renderChart(t *testing.T, sets ...string) []k8sDoc {
	t.Helper()
	args := []string{"template", "rel", filepath.Join("..", "..", "deploy", "helm", BinaryName), "--set", "config.network=net_ABC", "--set", "enrollment.url=https://app.example.com"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := helm(t, args...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var docs []k8sDoc
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var d k8sDoc
		if err := dec.Decode(&d); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("rendered YAML: %v", err)
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs
}

func find(docs []k8sDoc, kind string) k8sDoc {
	for _, d := range docs {
		if d["kind"] == kind {
			return d
		}
	}
	return nil
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func TestHelmChartRendersAHardenedStatefulSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helm tests run on unix")
	}
	if out, err := helm(t, "lint", filepath.Join("..", "..", "deploy", "helm", BinaryName), "--set", "config.network=net_ABC", "--set", "enrollment.url=https://app.example.com", "--set", "enrollment.token=nxt_ABCDEFGHJKLMNP"); err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
	docs := renderChart(t, "enrollment.token=nxt_ABCDEFGHJKLMNP", "config.targets[0].name=erp", "config.targets[0].kind=http", "config.targets[0].url=https://erp.internal", "config.targets[0].methods[0]=GET", "config.targets[0].paths[0]=/**")
	ss := find(docs, "StatefulSet")
	if ss == nil {
		t.Fatal("no StatefulSet")
	}
	if dig(ss, "spec", "replicas") != 2 {
		t.Errorf("replicas = %v", dig(ss, "spec", "replicas"))
	}
	podSpec := dig(ss, "spec", "template", "spec").(map[string]any)
	if dig(podSpec, "securityContext", "runAsNonRoot") != true || dig(podSpec, "automountServiceAccountToken") != false {
		t.Errorf("pod security = %v", podSpec["securityContext"])
	}
	c := podSpec["containers"].([]any)[0].(map[string]any)
	if dig(c, "securityContext", "readOnlyRootFilesystem") != true || dig(c, "securityContext", "allowPrivilegeEscalation") != false {
		t.Errorf("container security = %v", c["securityContext"])
	}
	if dig(c, "readinessProbe", "httpGet", "path") != "/readyz" || dig(c, "livenessProbe", "httpGet", "path") != "/healthz" {
		t.Error("probes")
	}
	if dig(podSpec, "affinity", "podAntiAffinity", "preferredDuringSchedulingIgnoredDuringExecution") == nil {
		t.Error("no anti-affinity")
	}
	if find(docs, "PodDisruptionBudget") == nil {
		t.Error("no PodDisruptionBudget")
	}
	if dig(ss, "spec", "volumeClaimTemplates") == nil {
		t.Error("the identity needs a volume of its own per pod")
	}
	// The rendered config is one the connector accepts.
	cm := find(docs, "ConfigMap")
	cfgText, _ := dig(cm, "data", "config.yaml").(string)
	cfg, err := ParseConfig([]byte(cfgText))
	if err != nil {
		t.Fatalf("rendered config: %v\n%s", err, cfgText)
	}
	if cfg.Network != "net_ABC" || len(cfg.Targets) != 1 || cfg.Health.Listen != "0.0.0.0:9464" {
		t.Errorf("config = %+v", cfg)
	}
	if find(docs, "Secret") == nil {
		t.Error("the token was not stored in a Secret")
	}
}

func TestHelmChartRequiredValuesAndExistingSecret(t *testing.T) {
	if _, err := helm(t, "template", "rel", filepath.Join("..", "..", "deploy", "helm", BinaryName)); err == nil {
		t.Error("the chart rendered without a network")
	}
	chart := filepath.Join("..", "..", "deploy", "helm", BinaryName)
	if out, err := helm(t, "template", "rel", chart, "--set", "config.network=net_ABC", "--set", "enrollment.url=https://app.example.com"); err == nil || !strings.Contains(out, "enrollment.token") {
		t.Errorf("no token and no secret: %v %s", err, out)
	}
	docs := renderChart(t, "enrollment.existingSecret=my-token", "replicaCount=1", "antiAffinity=hard", "secretMounts[0].name=erp-token", "secretMounts[0].secretName=erp-token")
	if find(docs, "Secret") != nil {
		t.Error("a Secret was created although one was named")
	}
	if find(docs, "PodDisruptionBudget") != nil {
		t.Error("a PodDisruptionBudget for a single replica")
	}
	ss := find(docs, "StatefulSet")
	c := dig(ss, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	env := c["env"].([]any)
	var ref string
	for _, e := range env {
		m := e.(map[string]any)
		if m["name"] == "ZUANO_CONNECTOR_ENROLL_TOKEN" {
			ref, _ = dig(m, "valueFrom", "secretKeyRef", "name").(string)
		}
	}
	if ref != "my-token" {
		t.Errorf("token secret = %q", ref)
	}
	if dig(ss, "spec", "template", "spec", "affinity", "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution") == nil {
		t.Error("hard anti-affinity")
	}
	mounts, _ := yaml.Marshal(c["volumeMounts"])
	if !strings.Contains(string(mounts), "/run/secrets/erp-token") {
		t.Errorf("mounts:\n%s", mounts)
	}
}
