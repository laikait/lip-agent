package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTheFileIsWrittenWholeAndReadableByItsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "laika-agent", "agent.json")

	if _, err := Load(path); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("no file yet: %v", err)
	}

	saved := Config{URL: "https://platform.example/api/agent/v1", AgentID: 3, ServerID: 7, ServerName: "Web one", Credential: "lia_abcdefgh1234"}
	if err := Save(path, saved); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil || loaded.ServerID != 7 || loaded.Credential != saved.Credential {
		t.Fatalf("%+v %v", loaded, err)
	}

	if loaded.Hint() != "…1234" {
		t.Fatal(loaded.Hint())
	}

	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		dir, _ := os.Stat(filepath.Dir(path))

		if info.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
			t.Fatalf("file %v, directory %v", info.Mode(), dir.Mode())
		}
	}

	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatal("a temporary file was left behind")
	}

	if err := os.WriteFile(path, []byte(`{"url":"https://x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("a file with no credential is refused")
	}
}
