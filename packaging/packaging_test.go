// Package packaging holds how the agent is installed. This test keeps the
// systemd unit that install.sh writes the same as packaging/laika-agent.service.
package packaging

import (
	"os"
	"strings"
	"testing"
)

func TestTheInstallerWritesTheUnitAsItIs(t *testing.T) {
	unit, err := os.ReadFile("laika-agent.service")
	if err != nil {
		t.Fatal(err)
	}

	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}

	text := strings.ReplaceAll(string(script), "\r\n", "\n")
	_, rest, found := strings.Cut(text, "<<'UNIT'\n")
	embedded, _, closed := strings.Cut(rest, "\nUNIT\n")

	if !found || !closed {
		t.Fatal("install.sh has no UNIT heredoc")
	}

	if embedded+"\n" != strings.ReplaceAll(string(unit), "\r\n", "\n") {
		t.Fatal("install.sh writes a different unit from laika-agent.service")
	}
}

func TestThePackageUpdateUnitTakesTheNameAsOneArgumentAndRunsNoShell(t *testing.T) {
	unit, err := os.ReadFile("laika-package-update@.service")
	if err != nil {
		t.Fatal(err)
	}

	text := string(unit)

	// %I is the unescaped instance, and systemd hands it over as one word.
	if !strings.Contains(text, "-- %I\n") || strings.Contains(text, "%i") {
		t.Fatal("the package name must reach the package manager as the last argument after --, unescaped (%I)")
	}

	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Exec") && (strings.Contains(line, "sh -c") || strings.Contains(line, "bash") || strings.Contains(line, ";") || strings.Contains(line, "&&")) {
			t.Fatalf("a shell in %q", line)
		}
	}
}
