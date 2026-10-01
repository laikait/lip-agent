package collect

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putFile(t *testing.T, root, name, content string) {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(name))

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintsAreHashAndSizeOfOnlyTheListedFiles(t *testing.T) {
	root := t.TempDir()
	putFile(t, root, "etc/nginx/nginx.conf", "worker_processes 2;\n")
	putFile(t, root, "etc/php/fpm/pool.d/www.conf", "pm = dynamic\n")
	putFile(t, root, "etc/php/fpm/pool.d/shop.conf", "pm = static\n")
	putFile(t, root, "etc/shadow", "root:secret\n")

	got, err := Fingerprints(root, []string{"/etc/nginx/nginx.conf", "/etc/php/fpm/pool.d/*.conf", "/etc/nginx/missing.conf"})
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte("worker_processes 2;\n"))
	want := map[string]Fingerprint{
		"/etc/nginx/nginx.conf":         {Path: "/etc/nginx/nginx.conf", SHA256: hex.EncodeToString(sum[:]), Size: 20},
		"/etc/php/fpm/pool.d/shop.conf": {Path: "/etc/php/fpm/pool.d/shop.conf"},
		"/etc/php/fpm/pool.d/www.conf":  {Path: "/etc/php/fpm/pool.d/www.conf"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %+v, want the three listed files and nothing else (not /etc/shadow, not the missing one)", got)
	}

	for i, fingerprint := range got {
		if i > 0 && got[i-1].Path >= fingerprint.Path {
			t.Fatalf("not sorted by path: %+v", got)
		}

		if fingerprint.Path == "/etc/nginx/nginx.conf" && fingerprint != want[fingerprint.Path] {
			t.Fatalf("%+v, want %+v", fingerprint, want[fingerprint.Path])
		}

		if len(fingerprint.SHA256) != 64 {
			t.Fatalf("%s has no SHA-256: %+v", fingerprint.Path, fingerprint)
		}
	}
}

func TestAFileThatChangedHasAnotherFingerprint(t *testing.T) {
	root := t.TempDir()
	putFile(t, root, "etc/app.conf", "a=1\n")
	before, _ := Fingerprints(root, []string{"/etc/app.conf"})

	putFile(t, root, "etc/app.conf", "a=2\n")
	after, _ := Fingerprints(root, []string{"/etc/app.conf"})

	if before[0].SHA256 == after[0].SHA256 || before[0].Size != after[0].Size {
		t.Fatalf("same size, different content should differ by hash: %+v %+v", before, after)
	}
}

func TestOnlyAbsolutePathsThatStayWhereTheyAreAreRead(t *testing.T) {
	root := t.TempDir()
	putFile(t, root, "etc/app.conf", "a=1\n")

	for _, pattern := range []string{"etc/app.conf", "/etc/../etc/app.conf", "../etc/app.conf"} {
		if _, err := Fingerprints(root, []string{pattern}); err == nil {
			t.Errorf("%q should be refused", pattern)
		}
	}
}

func TestADirectoryOrAHugeFileIsNotFingerprinted(t *testing.T) {
	root := t.TempDir()
	putFile(t, root, "etc/dir/inner.conf", "x\n")
	putFile(t, root, "etc/big.log", strings.Repeat("x", FileMaxSize+1))

	got, err := Fingerprints(root, []string{"/etc/dir", "/etc/big.log"})
	if err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}
