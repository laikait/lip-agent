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
