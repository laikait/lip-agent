package protocol

import (
	"bytes"
	"context"
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

	// The inventory, from the fixture host and the parsers' own inputs. What
	// depends on the machine running this test (its architecture, its
	// virtualisation, its addresses) is fixed, as the disk is above.
	hardware := sampler.Hardware(context.Background())
	hardware.Architecture, hardware.Virtualization = "x86_64", "kvm"

	jobs, err := collect.CronJobs(root)
	if err != nil {
		t.Fatal(err)
	}

	inventory := Inventory{
		InventoryID: "9a8b7c6d5e4f30211203f4e5d6c7b8a9",
		Hardware:    &hardware,
		Addresses:   []collect.Address{{Interface: "eth0", Address: "203.0.113.10"}, {Interface: "eth0", Address: "2001:db8::10"}},
		Packages:    collect.ParseDpkg([]byte("ii \tnginx\t1.22.1-9\tamd64\nii \topenssl\t3.0.11-1~deb12u2\tamd64\nrc \tapache2\t2.4.57-2\tamd64\n")),
		CronJobs:    &jobs,
		Timers:      collect.ParseTimers([]byte("Id=logrotate.timer\nUnit=logrotate.service\nTimersMonotonic=\nTimersCalendar={ OnCalendar=*-*-* 00:00:00 ; next_elapse=n/a }\n")),
	}

	zero := 0

	recordings := map[string]any{
		"register.json": Registration{
			Hostname:     host.Hostname,
			OS:           host.OS,
			Kernel:       host.Kernel,
			Version:      "0.3.0",
			Capabilities: []string{"metrics.read", "service.status", "inventory.read"},
		},
		"heartbeat.json": Heartbeat{Version: "0.3.0", Capabilities: []string{"metrics.read", "service.status", "inventory.read"}},
		// A machine whose file allows two operations says so in its heartbeat.
		"heartbeat-operations.json": Heartbeat{Version: "0.3.0", Capabilities: []string{"metrics.read", "service.status", "inventory.read", "service.restart", "package.status", "package.update", "backup.create", "config.fingerprint"}},
		// What it says of a command it did.
		"result.json": CommandResult{Status: "succeeded", ExitCode: &zero, Output: "restarted nginx.service; it is active"},
		// A machine whose file lists configuration files sends their fingerprints
		// (a hash and a size), which is all of them the platform ever holds.
		"inventory-files.json": Inventory{
			InventoryID: "5c4d3e2f1a0b99887766554433221100",
			Files: &[]collect.Fingerprint{
				{Path: "/etc/nginx/nginx.conf", SHA256: "3f2a91c04d1e5b7a8c9d0e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d", Size: 1204},
				{Path: "/etc/php/8.3/fpm/pool.d/www.conf", SHA256: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90", Size: 512},
			},
		},
		"metrics.json":   Batch{BatchID: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Host: &host, Samples: samples, Services: services},
		"inventory.json": inventory,
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
