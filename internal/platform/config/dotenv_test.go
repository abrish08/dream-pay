package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600)
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte(
		"# comment\nPG_TEST_A=plain\nPG_TEST_B=\"quoted <value>\"\nexport PG_TEST_C=exported\nPG_TEST_D=from-file\n"), 0o600)
	sub := filepath.Join(root, "cmd", "app")
	_ = os.MkdirAll(sub, 0o700)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	_ = os.Chdir(sub) // run from a sub-folder, like `go run main.go`

	t.Setenv("ENV_FILE", "")
	t.Setenv("PG_TEST_D", "from-environment")
	for _, k := range []string{"PG_TEST_A", "PG_TEST_B", "PG_TEST_C"} {
		os.Unsetenv(k)
		defer os.Unsetenv(k)
	}
	path, err := LoadDotEnv()
	if err != nil || filepath.Base(path) != ".env" {
		t.Fatalf("load: %q %v", path, err)
	}
	for k, want := range map[string]string{"PG_TEST_A": "plain", "PG_TEST_B": "quoted <value>",
		"PG_TEST_C": "exported", "PG_TEST_D": "from-environment"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	t.Setenv("ENV_FILE", "none")
	if p, _ := LoadDotEnv(); p != "" {
		t.Fatal("ENV_FILE=none must disable loading")
	}
}
