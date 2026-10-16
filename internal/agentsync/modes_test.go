package agentsync

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Agent JSON configs can hold environment values and hook commands, so a file
// zuano creates is private no matter which kind created it first.
func TestCreatedConfigFilesArePrivateForEveryKind(t *testing.T) {
	for _, kind := range []Kind{KindMCP, KindEnv, KindHooks} {
		env := newEnv(t)
		opts := userOpts(Claude)
		opts.Kinds = []Kind{kind}
		syncAll(t, env, userBundle(fixtureBundle()), opts)

		var checked int
		for _, rel := range []string{".claude.json", filepath.Join(".claude", "settings.json")} {
			info, err := os.Stat(filepath.Join(env.Home, rel))
			if err != nil {
				continue
			}
			checked++
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s created by kind %s has mode %v, want 0600", rel, kind, info.Mode().Perm())
			}
		}
		if checked == 0 {
			t.Errorf("kind %s created no config file", kind)
		}
	}
}

func TestExistingConfigModeIsPreserved(t *testing.T) {
	env := newEnv(t)
	path := filepath.Join(env.Home, ".claude", "settings.json")
	writeFile(t, path, "{}\n")
	if err := os.Chmod(path, fs.FileMode(0o640)); err != nil {
		t.Fatal(err)
	}
	opts := userOpts(Claude)
	opts.Kinds = []Kind{KindHooks}
	syncAll(t, env, userBundle(fixtureBundle()), opts)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want the user's 0640", info.Mode().Perm())
	}
}
