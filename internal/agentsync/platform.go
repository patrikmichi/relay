package agentsync

import (
	"path/filepath"
	"runtime"
)

// hostOS is the running OS; tests set it to cover the other platforms.
var hostOS = runtime.GOOS

// xdgConfig is $XDG_CONFIG_HOME, or ~/.config when it is unset.
func (e Env) xdgConfig() string {
	if x := e.getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	return filepath.Join(e.Home, ".config")
}

// appData is the Windows roaming application data directory.
func (e Env) appData() string {
	if a := e.getenv("APPDATA"); a != "" {
		return a
	}
	return filepath.Join(e.Home, "AppData", "Roaming")
}

func opencodeDir(env Env) string { return filepath.Join(env.xdgConfig(), "opencode") }

// vscodeGlobalStorage is where VS Code keeps extension state on this OS.
func vscodeGlobalStorage(env Env) string {
	switch hostOS {
	case "darwin":
		return filepath.Join(env.Home, "Library", "Application Support", "Code", "User", "globalStorage")
	case "windows":
		return filepath.Join(env.appData(), "Code", "User", "globalStorage")
	}
	return filepath.Join(env.xdgConfig(), "Code", "User", "globalStorage")
}

// devinConfigDir is Windsurf's (Devin's) per-user config directory.
func devinConfigDir(env Env) string {
	if hostOS == "windows" {
		return filepath.Join(env.appData(), "devin")
	}
	return filepath.Join(env.xdgConfig(), "devin")
}
