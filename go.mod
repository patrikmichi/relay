module github.com/patrikmichi/relay

// Compiler support policy: track the latest two upstream Go major
// releases (https://go.dev/doc/devel/release#policy). Bump the `go` line
// within one release cycle of a new major Go release, and keep `toolchain`
// pinned to that major's latest patch release so CI always builds with a
// current, security-patched compiler. ci.yml/release.yml both resolve their
// Go version from this file (go-version-file) so they can't drift apart.
go 1.26.0

toolchain go1.26.8

require (
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c
	github.com/spf13/cobra v1.10.2
	github.com/zalando/go-keyring v0.2.8
	golang.org/x/sys v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)
