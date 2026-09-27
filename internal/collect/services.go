package collect

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Service is one systemd service as the platform takes it: a name and one
// of running, stopped, failed or unknown.
type Service struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

const (
	// ServicesMax is the most the protocol takes in one batch.
	ServicesMax = 200

	serviceNameMax = 100
)

// order puts what somebody is looking for first when the list is cut.
var order = map[string]int{"failed": 0, "stopped": 1, "unknown": 2, "running": 3}

// ParseSystemctl reads `systemctl list-units --type=service --all
// --no-legend --plain`: UNIT LOAD ACTIVE SUB DESCRIPTION.
//
// With no names wanted, it reports the services that are doing something or
// have failed: loaded, and active (a daemon, not a one-shot that has
// exited), activating or failed. A server has dozens of static units, and
// "stopped" beside each is noise. Wanted names are reported whatever their
// state, and one that systemd does not know is "unknown".
func ParseSystemctl(output []byte, wanted []string) []Service {
	type unit struct{ load, active, sub string }

	units := map[string]unit{}

	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)

		// Some systemd versions mark failed units with a bullet even in plain mode.
		if len(fields) > 0 && (fields[0] == "●" || fields[0] == "*") {
			fields = fields[1:]
		}

		if len(fields) < 4 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}

		units[strings.TrimSuffix(fields[0], ".service")] = unit{fields[1], fields[2], fields[3]}
	}

	var services []Service

	if len(wanted) > 0 {
		seen := map[string]bool{}

		for _, name := range wanted {
			name = strings.TrimSuffix(strings.TrimSpace(name), ".service")
			if name == "" || seen[name] || len(name) > serviceNameMax {
				continue
			}

			seen[name] = true
			state := "unknown"

			if u, ok := units[name]; ok && u.load == "loaded" {
				state = stateOf(u.active, u.sub)
			}

			services = append(services, Service{Name: name, State: state})
		}
	} else {
		for name, u := range units {
			if u.load != "loaded" || len(name) > serviceNameMax {
				continue
			}

			if (u.active == "active" && u.sub != "exited") || u.active == "failed" || u.active == "activating" {
				services = append(services, Service{Name: name, State: stateOf(u.active, u.sub)})
			}
		}
	}

	sort.Slice(services, func(i, j int) bool {
		if order[services[i].State] != order[services[j].State] {
			return order[services[i].State] < order[services[j].State]
		}

		return services[i].Name < services[j].Name
	})

	if len(services) > ServicesMax {
		services = services[:ServicesMax]
	}

	return services
}

func stateOf(active, sub string) string {
	switch active {
	case "active", "reloading":
		return "running"
	case "failed":
		return "failed"
	case "inactive":
		return "stopped"
	default:
		// activating (often a unit restarting in a loop), deactivating.
		return "unknown"
	}
}

// ErrNoSystemd means there is no systemctl to ask: the machine reports no
// services, and says it cannot.
var ErrNoSystemd = errors.New("systemctl is not available")

// Services asks systemctl, directly and never through a shell, with a
// deadline: a hung D-Bus must not stop the metrics.
func Services(ctx context.Context, wanted []string) ([]Service, error) {
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, ErrNoSystemd
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, path, "list-units", "--type=service", "--all", "--no-legend", "--plain", "--no-pager").Output()
	if err != nil {
		return nil, err
	}

	return ParseSystemctl(output, wanted), nil
}
