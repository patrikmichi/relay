package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/patrikmichi/zuano/internal/textsafe"
	"github.com/patrikmichi/zuano/internal/webhooksign"
)

// webhookSecretEnv holds the secret forwarded events are signed with.
const webhookSecretEnv = "ZUANO_WEBHOOK_SECRET"

// DevCmd returns `zuano dev`, helpers for building against the gateway.
func DevCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dev", Short: "Develop against the gateway on your machine"}
	triggers := &cobra.Command{Use: "triggers", Short: "Work with trigger events locally"}
	triggers.AddCommand(devTriggersListenCmd())
	cmd.AddCommand(triggers)
	return cmd
}

type forwarder struct {
	url    *url.URL
	secret string
	client *http.Client
	out    io.Writer
	now    func() time.Time
}

func newForwarder(target string, allowRemote bool, secret string, out io.Writer) (*forwarder, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("--forward %q must be an http(s) URL without credentials", textsafe.Clean(target))
	}
	if !allowRemote && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("--forward %s is not on this machine; events carry your data, so a remote URL needs --allow-remote", textsafe.Clean(u.Host))
	}
	return &forwarder{url: u, secret: secret, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, out: out, now: time.Now}, nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// send posts one event envelope, signed as the gateway signs a delivery.
func (f *forwarder) send(ctx context.Context, body []byte) error {
	var env struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.ID == "" {
		return errors.New("the event is not an envelope with an id")
	}
	hdr, err := webhooksign.Sign(f.secret, env.ID, f.now(), body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = hdr
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := f.client.Do(req)
	if err != nil {
		fmt.Fprintf(f.out, "%s %s -> %s\n", env.Type, env.ID, textsafe.Clean(unwrapURLError(err)))
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	fmt.Fprintf(f.out, "%s %s -> %d (%s)\n", env.Type, env.ID, resp.StatusCode, time.Since(start).Round(time.Millisecond))
	return nil
}

func unwrapURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func devTriggersListenCmd() *cobra.Command {
	var f approvalFlags
	var tf triggerFilters
	var forward, outFile, replay string
	var allowRemote bool
	cmd := &cobra.Command{
		Use:   "listen --forward URL",
		Short: "Send trigger events to a server on your machine, signed like a real delivery",
		Long: `Follows the gateway's event stream with your API key and POSTs every event to
a local URL, signed exactly as a production delivery is (Standard Webhooks:
webhook-id, webhook-timestamp and webhook-signature), so the code that verifies
and parses deliveries runs end to end.

The signing secret is $` + webhookSecretEnv + ` (whsec_ and base64). Without it a new secret is
generated and printed once; give it to your server as the secret to verify with.

--out also appends each event to a JSONL file, and --replay sends the events of
such a file again (same ids) without opening the stream. Events are not sent to
a host other than this machine unless you pass --allow-remote.`,
		Example: `  zuano dev triggers listen --forward http://localhost:8000/hook --service github
  zuano dev triggers listen --forward http://localhost:8000/hook --replay events.jsonl`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if forward == "" {
				return errors.New("--forward names the URL to send events to")
			}
			secret := os.Getenv(webhookSecretEnv)
			errOut := cmd.ErrOrStderr()
			if secret == "" {
				var err error
				if secret, err = webhooksign.GenerateSecret(); err != nil {
					return err
				}
				fmt.Fprintf(errOut, "Signing secret (shown once): %s\nUse it to verify the events, or set %s to reuse one.\n", secret, webhookSecretEnv)
			} else if _, err := webhooksign.Sign(secret, "check", time.Now(), nil); err != nil {
				return fmt.Errorf("$%s: %w", webhookSecretEnv, err)
			}
			tee := io.Discard
			if outFile != "" {
				fh, err := os.OpenFile(outFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				defer fh.Close()
				tee = fh
			}
			fw, err := newForwarder(forward, allowRemote, secret, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if replay != "" {
				return replayEvents(cmd.Context(), fw, replay)
			}
			c, err := approvalsClient(f.gatewayURL)
			if err != nil {
				return err
			}
			fmt.Fprintf(errOut, "Forwarding events to %s. Stop with Ctrl+C.\n", textsafe.Clean(fw.url.String()))
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			handle := func(ev sseEvent) error {
				var line bytes.Buffer
				if err := json.Compact(&line, ev.Data); err != nil {
					return fmt.Errorf("the gateway sent an event that is not JSON: %w", err)
				}
				line.WriteByte('\n')
				_, _ = tee.Write(line.Bytes())
				return fw.send(ctx, bytes.TrimSpace(line.Bytes()))
			}
			return streamTriggerEvents(ctx, c, tf, "", handle, func(s string) { fmt.Fprintln(errOut, s) })
		},
	}
	f.bind(cmd, "Unused: events are sent, not printed as JSON")
	_ = cmd.Flags().MarkHidden("json")
	tf.bind(cmd, "trigger-id", "user-id")
	cmd.Flags().StringVar(&forward, "forward", "", "URL to POST each event to")
	cmd.Flags().StringVar(&outFile, "out", "", "Also append the events to this JSONL file")
	cmd.Flags().StringVar(&replay, "replay", "", "Send the events of this JSONL file again instead of following the stream")
	cmd.Flags().BoolVar(&allowRemote, "allow-remote", false, "Allow a URL that is not on this machine")
	return cmd
}

func replayEvents(ctx context.Context, fw *forwarder, path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		n++
		if err := fw.send(ctx, append([]byte(nil), line...)); err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s holds no events", textsafe.Clean(path))
	}
	return nil
}
