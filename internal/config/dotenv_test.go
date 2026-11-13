package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\n" +
		"DOTENV_JQL=assignee = currentUser() AND resolution = Unresolved\n" +
		"DOTENV_QUOTED=\"a b\"\n" +
		"export DOTENV_EXPORTED=1\n" +
		"DOTENV_PRESET=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOTENV_PRESET", "from-env")
	for _, k := range []string{"DOTENV_JQL", "DOTENV_QUOTED", "DOTENV_EXPORTED"} {
		t.Setenv(k, "") // register cleanup
		os.Unsetenv(k)
	}

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DOTENV_JQL":      "assignee = currentUser() AND resolution = Unresolved",
		"DOTENV_QUOTED":   "a b",
		"DOTENV_EXPORTED": "1",
		"DOTENV_PRESET":   "from-env",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("missing file should be ignored: %v", err)
	}
}
