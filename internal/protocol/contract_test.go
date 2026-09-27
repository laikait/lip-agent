package protocol

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/laikait/lip-agent/internal/collect"
)

// The contract: what this agent sends, recorded in testdata/contract/v1.
//
// The payloads are made by the agent's own code from the fixture host
// (testdata/host), and must equal the recordings. The platform's repository
// keeps a copy and replays them against its endpoints
// (tests/Platform/Feature/AgentContractTest.php), so a change on either side
// that breaks the other fails a test. After a deliberate change to what the
// agent sends:
//
//	go test ./internal/protocol -run Contract -update
//
// and copy testdata/contract/v1/*.json to the platform's
// tests/Platform/Fixtures/AgentContract/v1/.
var update = flag.Bool("update", false, "rewrite the contract recordings")

func TestContract(t *testing.T) {
	root := t.TempDir()
	copyHost(t, "../../testdata/host", root)

	sampler := collect.NewSampler(root, "/")
	host := sampler.Host()
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

	if _, _, err := sampler.Sample(start); err != nil {
		t.Fatal(err)
	}

	var samples []collect.Sample

	for minute, stat := range []string{
		"cpu  4765 356 604 3699196 23060 0 277 0 0 0\n",
		"cpu  4865 356 624 3699276 23080 0 277 0 0 0\n",
	} {
		writeFile(t, filepath.Join(root, "proc/stat"), stat)
		writeFile(t, filepath.Join(root, "proc/net/dev"), "Inter-|\n face |\n  eth0: "+[]string{"1600000", "2200000"}[minute]+" 0 0 0 0 0 0 0 "+[]string{"800000", "1100000"}[minute]+" 0 0 0 0 0 0 0\n")

		sample, ok, err := sampler.Sample(start.Add(time.Duration(minute+1) * time.Minute))
		if err != nil || !ok {
			t.Fatal(ok, err)
		}

		// Off Linux there is no disk; the recording holds a real one.
		sample.DiskUsed, sample.DiskTotal = 21474836480, 85899345920
		samples = append(samples, sample)
	}

	services := collect.ParseSystemctl([]byte("nginx.service loaded active running Web server\n● backup.service loaded failed failed Nightly backup\n"), nil)

	recordings := map[string]any{
		"register.json": Registration{
			Hostname:     host.Hostname,
			OS:           host.OS,
			Kernel:       host.Kernel,
			Version:      "0.1.0",
			Capabilities: []string{"metrics.read", "service.status"},
		},
		"heartbeat.json": Heartbeat{Version: "0.1.0", Capabilities: []string{"metrics.read", "service.status"}},
		"metrics.json":   Batch{BatchID: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Host: &host, Samples: samples, Services: services},
	}

	for name, payload := range recordings {
		got, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			t.Fatal(err)
		}

		got = append(got, '\n')
		path := filepath.Join("../../testdata/contract/v1", name)

		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}

			continue
		}

		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run with -update to record it)", name, err)
		}

		if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
			t.Errorf("%s differs from what the agent sends now:\n%s", name, got)
		}
	}
}

func copyHost(t *testing.T, from, to string) {
	t.Helper()

	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, _ := filepath.Rel(from, path)
		target := filepath.Join(to, relative)

		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
