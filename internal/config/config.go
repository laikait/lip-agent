// Package config is the enrolled agent's file: where the platform is, who
// the agent is, and its credential. Written once by `laika-agent enrol`,
// readable by its owner only.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPath is where the agent's file lives.
const DefaultPath = "/etc/laika-agent/agent.json"

// Config is the agent's file.
type Config struct {
	URL        string `json:"url"`
	AgentID    int64  `json:"agentId"`
	ServerID   int64  `json:"serverId"`
	ServerName string `json:"serverName"`
	Credential string `json:"credential"`

	// CAFile adds certificate authorities, for a platform behind a private one.
	CAFile string `json:"caFile,omitempty"`

	// Services names the services to report whatever their state. Empty
	// reports the ones running or failed.
	Services []string `json:"services,omitempty"`

	// Operations is what the platform may ask this machine to do: each
	// operation to the names it may be asked about, for example
	//
	//	"operations": {"service.restart": ["nginx", "php8.3-fpm"], "package.status": ["*"]}
	//
	// Nothing listed here is advertised to the platform, and nothing is done
	// that is not listed. "*" (any name) is for operations that change
	// nothing.
	Operations map[string][]string `json:"operations,omitempty"`

	// Files lists the configuration files whose fingerprint (SHA-256 and size,
	// never content) the platform may be told, so a change to one can be lined
	// up with an incident. An absolute path, with a wildcard allowed in its
	// file name:
	//
	//	"files": ["/etc/nginx/nginx.conf", "/etc/php/8.3/fpm/pool.d/*.conf"]
	//
	// Nothing outside this list is read, and nothing is advertised while it is
	// empty.
	Files []string `json:"files,omitempty"`

	// HostRoot is where the host's filesystem is, when the agent runs in a
	// container ("/host"). DiskPath is the filesystem reported, "/" by default.
	HostRoot string `json:"hostRoot,omitempty"`
	DiskPath string `json:"diskPath,omitempty"`
}

// ErrNotEnrolled means there is no file yet.
var ErrNotEnrolled = errors.New("this agent is not enrolled: run laika-agent enrol --url … --token …")

// Load reads the file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotEnrolled
	}

	if err != nil {
		return Config{}, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}

	if config.URL == "" || config.Credential == "" {
		return Config{}, fmt.Errorf("%s has no platform address or credential: enrol again", path)
	}

	return config, nil
}

// Save writes the file whole or not at all: a temporary file beside it,
// mode 0600 from the start, synced, then renamed over it. The directory is
// made 0700 if it is new.
func Save(path string, config Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".agent-*.json")
	if err != nil {
		return err
	}

	name := temporary.Name()
	defer os.Remove(name)

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()

		return err
	}

	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()

		return err
	}

	if err := temporary.Sync(); err != nil {
		temporary.Close()

		return err
	}

	if err := temporary.Close(); err != nil {
		return err
	}

	return os.Rename(name, path)
}

// Hint is the credential's last four characters: enough to tell two apart,
// never enough to use.
func (c Config) Hint() string {
	if len(c.Credential) < 4 {
		return "…"
	}

	return "…" + c.Credential[len(c.Credential)-4:]
}
