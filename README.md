# laika-agent

The server agent of the Laika Infrastructure Platform. It runs on a Linux
server and reports it: CPU, memory, load, disk, network, OS, kernel, uptime
and systemd services, and (from 0.2.0) its inventory: hardware, addresses,
installed packages, cron jobs and systemd timers.

- **Outbound only.** It calls the platform over HTTPS every minute, and never
  opens a port.
- **It runs nothing it is sent.** It advertises three named operations,
  `metrics.read`, `service.status` and `inventory.read`, and has no shell and
  no commands.
- **A cron line's arguments never leave the server.** It sends a job's
  schedule, user and program, and a SHA-256 of the whole line, so the platform
  sees that a line changed without seeing what it says.
- **One static binary**, for Linux on amd64 and arm64, with the standard library
  only.

The protocol is version 1, defined by the platform in its `plans/agent.md`.

## Install

On the platform, open **Servers → Add a server** and make a token. Then, on the
server:

```sh
sudo sh install.sh --url https://platform.example/api/agent/v1 --token lie_…
```

`install.sh` does the following:
- downloads the release for the machine's architecture and checks it against
  `SHA256SUMS` (or installs a local build with `--binary FILE`);
- installs it as `/usr/local/bin/laika-agent`;
- makes the system user `laika-agent`;
- installs the hardened `laika-agent` systemd service;
- enrols with the token, passed on standard input so it is not in the process
  list;
- starts the service.

`--uninstall` removes the agent; add `--purge` to remove its file too.

If the agent is already installed, enrol it by hand, as "Add a server" shows:

```sh
sudo laika-agent enrol --url https://platform.example/api/agent/v1 --token lie_…
sudo systemctl enable --now laika-agent
```

The token works once, within 24 hours. The agent swaps it for its own
credential, which is kept in `/etc/laika-agent/agent.json`. That file is mode
0600 and owned by `laika-agent`.

## Commands

| Command | What |
|---|---|
| `laika-agent enrol --url … --token …` | Register with a one-time token. `--token -` reads it from standard input. `--ca-file` adds certificate authorities. `--host-root /host` is for running in a container. `--force` enrols again. |
| `laika-agent run` | Report every minute, which is what the service runs. `--once` takes one sample, sends it and exits: a check that it works. |
| `laika-agent status` | Where it reports, as which server, and what it reads now, inventory included. |
| `laika-agent version` | The version, platform and protocol. |

Every command takes `--config FILE`.

**Exit codes:**
- 0: done.
- 1: an error.
- 2: wrong usage.
- 78: not enrolled, or the platform no longer accepts it. The server was
  removed, so enrol again. systemd does not restart the service on 78.

## How it behaves

- **Sampling:** every minute (or as the platform asks, between 15 seconds and an
  hour). CPU and network are measured over the minute. Memory counts reclaimable
  cache as free. Disk is the root filesystem.
- **Services:** the ones running or failed. To report particular ones whatever
  their state, list them under `services` in the agent's file.
- **When the platform cannot be reached:** samples are kept, up to six hours, and
  sent when it can be. The wait between tries grows from 30 seconds to 10 minutes.
- **Each batch has an id** and keeps it until the platform acknowledges it. A
  batch whose reply was lost is sent again under the same id, and the platform
  keeps it once.
- **A batch the platform refuses** (4xx) is dropped and logged.
- **A refused credential** (401) stops the agent.
- **Plain HTTP only to this machine.** The credential goes in a header, never
  the URL, and never to anywhere a redirect points.

## The inventory

Looked at when the agent starts, then every hour (or as the platform asks,
between 5 minutes and a day), and **sent only when something changed**, or once
a day so the platform knows it is current. Like a batch, it keeps its id until
the platform acknowledges it.

| Part | Where from |
|---|---|
| Hardware | cores from `/proc/cpuinfo`, memory from `/proc/meminfo`, the architecture, and `systemd-detect-virt` when there is one |
| Addresses | the interfaces that are up, without loopback and link-local addresses. In a container they are the container's unless it shares the host's network. |
| Packages | `dpkg-query`, `rpm` or `apk`, whichever answers, run directly (no shell) with a 30-second deadline. Installed ones only, at most 5,000. |
| Cron jobs | `/etc/crontab`, `/etc/cron.d/*` and the programs in `/etc/cron.{hourly,daily,weekly,monthly}`. Users' own crontabs in `/var/spool/cron` need root and are not read. |
| Timers | `systemctl list-units --type=timer` and `systemctl show`, run directly with a deadline |

- **A cron job's program** is the first word of its command, after any
  `NAME=value` assignments, whose values are never read. Debian's
  `[ -x /path ] && /path` is reported as `/path`, because a path tested with
  `-x` is a program. Any other test (`test -e /run/systemd/system || …`) is
  reported as `test`, and its path stays on the machine.
- **A part that cannot be read is left out** of the report, and the platform
  keeps what it knew of it. For example, with no package manager, packages are
  not reported. This is said once in the log.
- **Every value is cut to the platform's limits**, on a character boundary, so
  one long package name cannot get the whole report refused.
- **A platform older than the inventory call** answers 404. The agent then
  waits a day before it tries again, unless the machine changes first.
  Metrics are not affected.

## Develop

```sh
go test ./...
sh scripts/build.sh 0.2.0   # dist/: laika-agent-linux-{amd64,arm64}, SHA256SUMS, install.sh
```

The collectors are tested on any OS against `testdata/host`, a fixture root
(`/proc`, and `/etc` for cron).

**The contract:** `internal/protocol/contract_test.go` makes the register,
heartbeat, metrics and inventory payloads from that fixture and compares them with
`testdata/contract/v1/*.json`. The platform keeps a copy of those files in
`tests/Platform/Fixtures/AgentContract/v1/` and replays them against its
endpoints. It also keeps each earlier release's recordings, frozen in
`v1/<version>/` (`v1/0.1.0/` today), so an agent still in the field keeps
working. After a deliberate change to what the agent sends:
1. before releasing a new version, freeze the platform's current copies in
   `v1/<the version being replaced>/`;
2. run `go test ./internal/protocol -run Contract -update`;
3. copy the files across;
4. run the platform's tests.
