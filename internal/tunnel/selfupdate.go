package tunnel

import (
	"context"
	"crypto/ed25519"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// releasesURL is where the update manifest is read from.
func (a *Agent) releasesURL() string {
	if u := a.config().Update.URL; u != "" {
		return u
	}
	return os.Getenv("ZUANO_CONNECTOR_RELEASES_URL")
}

func (a *Agent) updateKeys() []ed25519.PublicKey {
	if len(a.opts.UpdateKeys) > 0 {
		return a.opts.UpdateKeys
	}
	return UpdateKeys()
}

// CheckForUpdate looks for a newer release without installing it.
func (a *Agent) CheckForUpdate(ctx context.Context) (*UpdateInfo, error) {
	url := a.releasesURL()
	if url == "" {
		return nil, errors.New("no releases address is configured (update.url)")
	}
	return CheckUpdate(ctx, a.opts.HTTP, url, a.config().Update.Channel, Version, a.updateKeys(), false)
}

// autoUpdateLoop installs a newer release when both the local config and the
// network allow it, inside the maintenance window, and never in a container.
func (a *Agent) autoUpdateLoop(ctx context.Context) {
	if a.opts.Executable == "" {
		return
	}
	for {
		jitter := time.Duration(rand.Int64N(int64(30 * time.Minute)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(6*time.Hour + jitter):
		}
		a.autoUpdateOnce(ctx)
	}
}

func (a *Agent) autoUpdateOnce(ctx context.Context) {
	cfg := a.config()
	if !cfg.Update.Auto || cfg.Update.Method != "binary" || InContainer() {
		return
	}
	if cfg.Update.Window != "" {
		if w, err := ParseWindow(cfg.Update.Window); err == nil && !w.Contains(a.now()) {
			return
		}
	}
	info, err := a.CheckForUpdate(ctx)
	if err != nil {
		a.log.Event("warn", "update_check_failed", map[string]any{"error": err.Error()})
		return
	}
	policy, _ := a.netAutoUpdate.Load().(string)
	if info == nil || !AutoUpdateAllowed(cfg.Update.Auto, policy, info.Class) {
		return
	}
	if err := a.installUpdate(ctx, info); err != nil {
		a.log.Event("error", "update_failed", map[string]any{"error": err.Error(), "version": info.Manifest.Version})
	}
}

// installUpdate downloads, verifies and swaps the binary, then asks Run to
// drain and return ErrRestart.
func (a *Agent) installUpdate(ctx context.Context, info *UpdateInfo) error {
	dir := filepath.Dir(a.opts.Executable)
	path, err := Download(ctx, a.opts.HTTP, info.Artifact, dir)
	if err != nil {
		return err
	}
	if err := Swap(a.opts.Executable, path); err != nil {
		_ = os.Remove(path)
		return err
	}
	a.log.Event("info", "updated", map[string]any{"version": info.Manifest.Version, "class": info.Class})
	a.restarting.Store(true)
	select {
	case a.restartCh <- struct{}{}:
	default:
	}
	return nil
}

// watchPending confirms a freshly swapped binary once it is connected, and
// goes back to the previous one when it is not within two minutes.
func (a *Agent) watchPending(ctx context.Context) {
	exe := a.opts.Executable
	if exe == "" || !Pending(exe) {
		return
	}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if a.ready.Load() {
				Confirm(exe)
				a.log.Event("info", "update_confirmed", map[string]any{"version": Version})
				return
			}
		case <-deadline.C:
			if err := Rollback(exe); err != nil {
				a.log.Event("error", "rollback_failed", map[string]any{"error": err.Error()})
				return
			}
			a.log.Event("error", "update_failed", map[string]any{"error": "the new version did not connect within two minutes; went back to the previous one"})
			a.restarting.Store(true)
			select {
			case a.restartCh <- struct{}{}:
			default:
			}
			return
		}
	}
}
