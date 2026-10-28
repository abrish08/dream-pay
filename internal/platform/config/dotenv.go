package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadDotEnv finds a .env file (in the current folder or any parent, up to
// the folder with go.mod) and sets the variables it contains, so `go run
// main.go` works from anywhere in the project.
//
//   - A variable already set in the environment always wins (production sets
//     real environment variables or secrets; .env is for development).
//   - ENV_FILE=path loads that file instead; ENV_FILE=none disables loading.
//
// Returns the file loaded ("" if none).
func LoadDotEnv() (string, error) {
	path := os.Getenv("ENV_FILE")
	switch path {
	case "none":
		return "", nil
	case "":
		path = findDotEnv()
		if path == "" {
			return "", nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return "", fmt.Errorf("%s:%d: expected KEY=value", path, n)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			_ = os.Setenv(key, val)
		}
	}
	return path, sc.Err()
}

func findDotEnv() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			return filepath.Join(dir, ".env")
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return "" // reached the project root without finding one
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
