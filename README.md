# laika-agent

The server agent of the Laika Infrastructure Platform. It runs on a Linux
server and reports it: CPU, memory, load, disk, network, OS, kernel, uptime
and systemd services, and (from 0.2.0) its inventory: hardware, addresses,
installed packages, cron jobs and systemd timers. (From 0.3.0, the few
operations its owner allows.)

- **Outbound only.** It calls the platform over HTTPS every minute, and never
  opens a port.
- **No shell, and no command line from the platform.** By default it advertises
  three reading operations, `metrics.read`, `service.status` and
  `inventory.read`, and does nothing else. From 0.3.0 the platform can ask for
  a named operation (`service.restart`, `package.status`, `package.update`,
  `backup.create`), but only for the names **this machine's own file allows** (see "Operations").
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

## Operations

From 0.3.0. The platform can ask this agent to restart a service, read or update a
package, or run a backup job, after its owner's policy and a second person's approval. The agent is
the last gate, and the machine's owner holds it: **nothing is advertised or
done unless `/etc/laika-agent/agent.json` lists it**.

```json
"operations": {
  "service.restart": ["nginx", "php8.3-fpm"],
  "package.status": ["*"],
  "package.update": ["openssl"],
  "backup.create": ["nightly-backup"]
}
```

- Each operation lists the names it may be asked about. `*` (any name) counts
  **only for `package.status`**, which reads; for the others it allows nothing. An operation with no list is not advertised, and is
  refused if asked for anyway.
- The platform sends an operation's name and one parameter (a service, a
  package). The agent checks it is a plain name (letters, digits and `@ + . _ : -`,
  starting with a letter or digit, at most 120), checks the file allows it, and
  builds the argument list itself: `systemctl restart --no-ask-password <unit>.service`,
  then `systemctl is-active`. **No shell, and never a string from the platform run
  as a command.**
- **Polling only:** every 15 seconds (as the platform asks) it does `GET
  /commands`. A command is handed over once; the agent runs it once (a repeat id
  is ignored), then answers `POST /commands/{id}/result` with success or failure,
  the exit code and up to 8 KB of output. The platform redacts the output. An
  expired command is not run. An agent allowed to do nothing does not poll.
- **Permission is the system's.** The service runs as the unprivileged
  `laika-agent` user with `NoNewPrivileges`, so it cannot use sudo. To let it
  restart exactly the units the file lists, give that user a polkit rule for the
  same names, for example `/etc/polkit-1/rules.d/50-laika-agent.rules`:

  ```js
  polkit.addRule(function(action, subject) {
      if (subject.user == "laika-agent" &&
          action.id == "org.freedesktop.systemd1.manage-units" &&
          ["nginx.service", "php8.3-fpm.service"].indexOf(action.lookup("unit")) >= 0) {
          return polkit.Result.YES;
      }
  });
  ```

  Without the rule the restart is refused by the system and the platform is told
  it failed, with the reason.
- **`package.update`** does not run a package manager as the agent. It starts
  the systemd template unit `laika-package-update@<package>.service` (the name
  escaped the way `systemd-escape` does), which you install from
  `packaging/laika-package-update@.service` (Debian and Ubuntu; the file says
  what to change for others) and which runs as root. Give the agent's user a
  polkit rule for the same packages: the unit for `openssl` is
  `laika-package-update@openssl.service`. The agent then reports the version
  now installed. Up to 10 minutes.
- **`backup.create`** starts one of your own backup jobs, a systemd service you
  wrote (`nightly-backup.service`), and waits for it to end; the job decides
  what a backup is. Allow it with a polkit rule for that unit. Up to 20
  minutes. The platform accepts the answer until 30 minutes after it queued the
  command, and the agent handles one command at a time.
- `laika-agent status` says what this machine may do.

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
| Files | only the files the machine's own `"files"` list names (see below): a SHA-256 and a size each |

### Configuration files: fingerprints only

So that a change to Nginx or PHP-FPM can be lined up with an incident, the
agent can tell the platform **that** a file changed, never **what it says**
(configuration holds passwords and keys). It is off until the machine's owner
lists files in `/etc/laika-agent/agent.json`:

```json
"files": ["/etc/nginx/nginx.conf", "/etc/php/8.3/fpm/pool.d/*.conf"]
```

- **Only what is listed is read.** A path is absolute, may have a wildcard in
  its file name, and may not go up a directory (`..` is refused). A directory
  or a file over 8 MiB is skipped. At most 200 files.
- **What is sent** per file: its path, the SHA-256 of its bytes, and its size.
  Nothing else, and never a line of it.
- **Nothing is advertised while the list is empty.** With files listed the
  agent advertises `config.fingerprint`.
- A listed file that is not there is left out of the report, and the platform
  records it as removed. A file that is there but cannot be read (permissions)
  makes the whole part unreported, said once in the log, so the platform keeps
  what it knew rather than seeing every file removed. Give the agent's user
  read access to the files it is meant to fingerprint.

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
sh scripts/build.sh 0.3.0   # dist/: laika-agent-linux-{amd64,arm64}, SHA256SUMS, install.sh
```

The collectors are tested on any OS against `testdata/host`, a fixture root
(`/proc`, and `/etc` for cron).

**The contract:** `internal/protocol/contract_test.go` makes the register,
heartbeat (with and without operations), metrics, inventory and command-result payloads from that fixture and compares them with
`testdata/contract/v1/*.json`. The platform keeps a copy of those files in
`tests/Platform/Fixtures/AgentContract/v1/` and replays them against its
endpoints. It also keeps each earlier release's recordings, frozen in
`v1/<version>/` (`v1/0.1.0/` and `v1/0.2.0/` today), so an agent still in the field keeps
working. After a deliberate change to what the agent sends:
1. before releasing a new version, freeze the platform's current copies in
   `v1/<the version being replaced>/`;
2. run `go test ./internal/protocol -run Contract -update`;
3. copy the files across;
4. run the platform's tests.
