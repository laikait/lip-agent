// laika-agent reports a server to the Laika Infrastructure Platform: its
// CPU, memory, load, disk, network and services, over HTTPS, outbound only.
// It runs no command it was sent and opens no port.
//
//	laika-agent enrol --url https://platform.example/api/agent/v1 --token lie_…
//	laika-agent run
//	laika-agent status
//	laika-agent version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/laikait/lip-agent/internal/agent"
	"github.com/laikait/lip-agent/internal/collect"
	"github.com/laikait/lip-agent/internal/config"
	"github.com/laikait/lip-agent/internal/protocol"
)

// version is set when a release is built: -ldflags "-X main.version=0.1.0".
var version = "0.1.0-dev"

const (
	exitError  = 1
	exitUsage  = 2
	exitConfig = 78 // EX_CONFIG: not enrolled, or no longer accepted. systemd does not restart on it.

	// serviceUser owns the agent's file when it exists (install.sh makes it).
	serviceUser = "laika-agent"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(exitUsage)
	}

	command, args := os.Args[1], os.Args[2:]

	var code int

	switch command {
	case "enrol", "enroll":
		code = enrol(args)
	case "run":
		code = run(args)
	case "status":
		code = status(args)
	case "version", "--version", "-v":
		fmt.Println(userAgent())
	case "help", "--help", "-h":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "laika-agent: no such command %q\n\n", command)
		usage(os.Stderr)
		code = exitUsage
	}

	os.Exit(code)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `laika-agent: reports this server to the Laika Infrastructure Platform.

  laika-agent enrol --url <platform>/api/agent/v1 --token lie_…
      Register this server with a one-time token from "Add a server", and
      keep the credential it is given in `+config.DefaultPath+`.
  laika-agent run [--once]
      Report every minute (what the systemd service runs). --once takes one
      sample, sends it, and exits.
  laika-agent status
      Say where it reports, as which server, and what it reads now.
  laika-agent version

Every command takes --config <file> (default `+config.DefaultPath+`).
`)
}

func enrol(args []string) int {
	flags := flag.NewFlagSet("enrol", flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "where to keep the agent's file")
	address := flags.String("url", "", "the platform's address, as \"Add a server\" shows it")
	token := flags.String("token", "", "the one-time enrolment token (lie_…), or - to read it from standard input")
	caFile := flags.String("ca-file", "", "extra certificate authorities, for a platform behind a private one")
	hostRoot := flags.String("host-root", "/", "where the host's filesystem is (/host in a container)")
	force := flags.Bool("force", false, "enrol again, replacing the existing file")

	if flags.Parse(args) != nil {
		return exitUsage
	}

	secret := *token
	if secret == "-" {
		read, err := io.ReadAll(io.LimitReader(os.Stdin, 1024))
		if err != nil {
			return fail(err)
		}

		secret = strings.TrimSpace(string(read))
	}

	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("LAIKA_ENROLMENT_TOKEN"))
	}

	if *address == "" || secret == "" {
		fmt.Fprintln(os.Stderr, "laika-agent enrol needs --url and --token, as \"Add a server\" shows them.")

		return exitUsage
	}

	if _, err := os.Stat(*path); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "%s exists: this server is already enrolled. Remove the server in the portal and enrol with --force to start again.\n", *path)

		return exitConfig
	}

	client, err := protocol.NewClient(*address, *caFile, userAgent())
	if err != nil {
		return fail(err)
	}

	sampler := collect.NewSampler(*hostRoot, "/")
	host := sampler.Host()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	registered, err := client.Register(ctx, secret, protocol.Registration{
		Hostname:     host.Hostname,
		OS:           host.OS,
		Kernel:       host.Kernel,
		Version:      version,
		Capabilities: agent.Capabilities,
	})

	if protocol.Unauthorized(err) {
		fmt.Fprintln(os.Stderr, "The platform refused the token: it is wrong, used, or more than 24 hours old. Make another in \"Add a server\".")

		return exitConfig
	}

	if err != nil {
		return fail(err)
	}

	saved := config.Config{
		URL:        client.BaseURL(),
		AgentID:    registered.AgentID,
		ServerID:   registered.ServerID,
		ServerName: registered.ServerName,
		Credential: registered.Credential,
		CAFile:     *caFile,
	}

	if *hostRoot != "/" {
		saved.HostRoot = *hostRoot
	}

	if err := config.Save(*path, saved); err != nil {
		// Registered, but the credential is lost: say what to do.
		fmt.Fprintf(os.Stderr, "Registered as server #%d, but %s could not be written: %v\nRemove the server in the portal and enrol again.\n", registered.ServerID, *path, err)

		return exitError
	}

	handOver(*path)

	fmt.Printf("Registered as \"%s\" (server #%d). The credential is in %s.\n", registered.ServerName, registered.ServerID, *path)
	fmt.Println("Start reporting: systemctl enable --now laika-agent")

	return 0
}

// handOver gives the agent's file to the service's user, when there is one,
// so the service need not run as root.
func handOver(path string) {
	account, err := user.Lookup(serviceUser)
	if err != nil {
		return
	}

	uid, err1 := strconv.Atoi(account.Uid)
	gid, err2 := strconv.Atoi(account.Gid)

	if err1 != nil || err2 != nil {
		return
	}

	_ = os.Chown(path, uid, gid)

	if dir := strings.TrimSuffix(path, "/agent.json"); dir != path {
		_ = os.Chown(dir, uid, gid)
	}
}

func run(args []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "the agent's file")
	once := flags.Bool("once", false, "one sample, sent, then exit")

	if flags.Parse(args) != nil {
		return exitUsage
	}

	logger := log.New(os.Stderr, "", 0)
	if os.Getenv("INVOCATION_ID") == "" {
		// Not under systemd, whose journal stamps the time itself.
		logger.SetFlags(log.LstdFlags)
	}

	saved, err := config.Load(*path)
	if err != nil {
		logger.Print(err)

		return exitConfig
	}

	client, err := protocol.NewClient(saved.URL, saved.CAFile, userAgent())
	if err != nil {
		logger.Print(err)

		return exitConfig
	}

	sampler := collect.NewSampler(saved.HostRoot, saved.DiskPath)
	runner := &agent.Runner{
		Platform: client.WithCredential(saved.Credential),
		Machine:  sampler,
		Services: func(ctx context.Context) ([]collect.Service, error) { return collect.Services(ctx, saved.Services) },
		Version:  version,
		Log:      logger,
	}

	logger.Printf("%s reporting to %s as server #%d (credential %s)", userAgent(), saved.URL, saved.ServerID, saved.Hint())

	if *once {
		return runOnce(runner, sampler, logger)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runner.Run(ctx); errors.Is(err, agent.ErrRevoked) {
		return exitConfig
	}

	logger.Print("stopped")

	return 0
}

// runOnce is a sample over five seconds, sent: a check that the agent can
// read this machine and reach the platform.
func runOnce(runner *agent.Runner, sampler *collect.Sampler, logger *log.Logger) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if _, _, err := sampler.Sample(time.Now()); err != nil {
		logger.Printf("cannot read this machine: %v", err)

		return exitError
	}

	time.Sleep(5 * time.Second)

	if err := runner.Heartbeat(ctx); err != nil {
		if errors.Is(err, agent.ErrRevoked) {
			return exitConfig
		}

		return exitError
	}

	if err := runner.Tick(ctx); err != nil {
		return exitError
	}

	logger.Print("sent: the platform has this server's sample")

	return 0
}

func status(args []string) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	path := flags.String("config", config.DefaultPath, "the agent's file")

	if flags.Parse(args) != nil {
		return exitUsage
	}

	saved, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return exitConfig
	}

	sampler := collect.NewSampler(saved.HostRoot, saved.DiskPath)
	host := sampler.Host()

	fmt.Printf("%s\n", userAgent())
	fmt.Printf("Platform:   %s\n", saved.URL)
	fmt.Printf("Server:     \"%s\" (#%d), agent #%d, credential %s\n", saved.ServerName, saved.ServerID, saved.AgentID, saved.Hint())
	fmt.Printf("Host:       %s · %s · %s · up %ds\n", host.Hostname, host.OS, host.Kernel, host.UptimeSeconds)

	if _, _, err := sampler.Sample(time.Now()); err != nil {
		fmt.Printf("Reading:    cannot read this machine: %v\n", err)

		return exitError
	}

	time.Sleep(time.Second)

	if sample, ok, err := sampler.Sample(time.Now()); err == nil && ok {
		fmt.Printf("Reading:    CPU %.1f%% · memory %d/%d MB · load %.2f · disk %d/%d GB\n",
			sample.CPU, sample.MemoryUsed>>20, sample.MemoryTotal>>20, sample.Load1, sample.DiskUsed>>30, sample.DiskTotal>>30)
	}

	services, err := collect.Services(context.Background(), saved.Services)
	if err != nil {
		fmt.Printf("Services:   not reported: %v\n", err)
	} else {
		fmt.Printf("Services:   %d reported\n", len(services))
	}

	return 0
}

func userAgent() string {
	return fmt.Sprintf("laika-agent/%s (%s; %s; protocol %d)", version, runtime.GOOS, runtime.GOARCH, protocol.Version)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "laika-agent:", err)

	return exitError
}
