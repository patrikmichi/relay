//go:build ignore

// sign-manifest writes manifest.json and manifest.json.sig for a connector
// release. Run it where the offline update key is available:
//
//	CONNECTOR_UPDATE_PRIVATE_KEY=<base64 ed25519 seed> \
//	  go run scripts/release/sign-manifest.go -dist dist -version 1.4.2 -base-url https://releases.example/connector/1.4.2 -out out
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/patrikmichi/zuano/internal/tunnel"
)

func main() {
	dist := flag.String("dist", "dist", "directory with the bare binaries (zuano-connector_<os>_<arch>)")
	version := flag.String("version", "", "release version, such as 1.4.2")
	base := flag.String("base-url", "", "where the binaries are published")
	out := flag.String("out", ".", "directory to write manifest.json and manifest.json.sig to")
	flag.Parse()
	seed, err := base64.StdEncoding.DecodeString(os.Getenv("CONNECTOR_UPDATE_PRIVATE_KEY"))
	if err != nil || len(seed) != ed25519.SeedSize || *version == "" || *base == "" {
		fmt.Fprintln(os.Stderr, "usage: CONNECTOR_UPDATE_PRIVATE_KEY=<base64 seed> go run scripts/release/sign-manifest.go -version V -base-url URL [-dist DIR] [-out DIR]")
		os.Exit(2)
	}
	m, err := tunnel.ManifestFromDist(*dist, tunnel.BinaryName, *version, *base, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	manifest, sig, err := tunnel.SignManifest(m, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, content := range map[string][]byte{"manifest.json": manifest, "manifest.json.sig": sig} {
		if err := os.WriteFile(filepath.Join(*out, name), content, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Printf("wrote %s and %s (%d artifacts)\n", filepath.Join(*out, "manifest.json"), filepath.Join(*out, "manifest.json.sig"), len(m.Artifacts))
}
