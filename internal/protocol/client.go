// Package protocol speaks version 1 of the agent protocol
// (plans/agent.md in the platform's repository): register, heartbeat,
// metrics and inventory, over HTTPS, with the credential in a header and
// never the URL.
package protocol

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/laikait/lip-agent/internal/collect"
)

const (
	// Version is the protocol version this agent speaks.
	Version = 1

	// BasePath is where the protocol lives on a platform.
	BasePath = "/api/agent/v1"

	// EnrolmentHeader carries the one-time token, to register.
	EnrolmentHeader = "X-Laika-Enrolment"

	// AgentHeader carries the agent's credential on every other call.
	AgentHeader = "X-Laika-Agent"

	// replyMax is the most of a reply read: the platform's are small.
	replyMax = 1 << 20
)

// Registration is what an agent says about itself when it registers.
type Registration struct {
	Hostname     string   `json:"hostname"`
	OS           string   `json:"os,omitempty"`
	Kernel       string   `json:"kernel,omitempty"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

// Registered is the platform's answer: who the agent is now, and its
// credential, which is given once.
type Registered struct {
	AgentID          int64    `json:"agentId"`
	ServerID         int64    `json:"serverId"`
	ServerName       string   `json:"serverName"`
	Credential       string   `json:"credential"`
	CredentialHeader string   `json:"credentialHeader"`
	Capabilities     []string `json:"capabilities"`
}

// Heartbeat says the agent is alive, and what it is now.
type Heartbeat struct {
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Batch is samples sent together, under an id the platform remembers, so a
// batch sent again after a lost reply is accepted once.
type Batch struct {
	BatchID  string            `json:"batchId"`
	Host     *collect.Host     `json:"host,omitempty"`
	Samples  []collect.Sample  `json:"samples"`
	Services []collect.Service `json:"services,omitempty"`
}

// Inventory is what the machine is and what runs on it, under an id the
// platform remembers, like a batch's. A list left out (nil) was not read:
// the platform keeps what it knew. CronJobs is a pointer so that "none"
// (an empty list) and "not read" (nil) stay different on the wire.
type Inventory struct {
	InventoryID string             `json:"inventoryId"`
	Hardware    *collect.Hardware  `json:"hardware,omitempty"`
	Addresses   []collect.Address  `json:"addresses,omitempty"`
	Packages    []collect.Package  `json:"packages,omitempty"`
	CronJobs    *[]collect.CronJob `json:"cronJobs,omitempty"`
	Timers      []collect.Timer    `json:"timers,omitempty"`
}

// Answer is what every reply carries: how often to call, and whether a
// batch was one already received. InventorySeconds is 0 from a platform
// older than the inventory call.
type Answer struct {
	HeartbeatSeconds int    `json:"heartbeatSeconds"`
	MetricsSeconds   int    `json:"metricsSeconds"`
	InventorySeconds int    `json:"inventorySeconds"`
	Protocol         int    `json:"protocol"`
	ServerTime       string `json:"serverTime"`
	Accepted         bool   `json:"accepted"`
	Duplicate        bool   `json:"duplicate"`
}

// Error is the platform refusing a call, in its error shape.
type Error struct {
	Status  int
	Title   string
	Message string
	Fields  map[string]string
}

func (e *Error) Error() string {
	message := e.Message
	if message == "" {
		message = e.Title
	}

	if len(e.Fields) > 0 {
		var parts []string
		for field, problem := range e.Fields {
			parts = append(parts, field+": "+problem)
		}

		message += " (" + strings.Join(parts, "; ") + ")"
	}

	return fmt.Sprintf("the platform answered %d: %s", e.Status, message)
}

// Unauthorized means the credential or token is not accepted: it was wrong,
// used, expired, or its server was removed. Retrying will not help.
func Unauthorized(err error) bool {
	var e *Error

	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// Rejected means the platform refused what was sent, for good: sending it
// again will be refused again. Rate limits and server errors are not this.
func Rejected(err error) bool {
	var e *Error

	return errors.As(err, &e) && e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && e.Status != http.StatusUnauthorized
}

// Client calls one platform.
type Client struct {
	base       string
	credential string
	userAgent  string
	http       *http.Client
}

// NewClient talks to the platform at address: its root
// (https://platform.example) or the protocol's base
// (https://platform.example/api/agent/v1), as the portal shows it.
//
// **HTTPS only**, with the certificate checked, except to this machine
// (localhost, 127.0.0.1, ::1), where a platform under development is plain
// HTTP. caFile adds certificate authorities for a platform behind a private
// one.
func NewClient(address, caFile, userAgent string) (*Client, error) {
	base, err := Base(address)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("the certificate authorities in %s: %w", caFile, err)
		}

		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}

		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificates", caFile)
		}

		transport.TLSClientConfig.RootCAs = pool
	}

	return &Client{
		base:      base,
		userAgent: userAgent,
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: transport,
			// A redirect is answered, never followed: Go would carry the
			// credential header to wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Base is the protocol's base URL for an address, checked.
func Base(address string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(address))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("%q is not the platform's address; it looks like https://platform.example%s", address, BasePath)
	}

	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("the platform's address takes no user, query or fragment")
	}

	switch parsed.Scheme {
	case "https":
	case "http":
		if !loopback(parsed.Hostname()) {
			return "", fmt.Errorf("%s is plain HTTP: the credential would cross the network readable. Use https://", address)
		}
	default:
		return "", fmt.Errorf("%q is not an http(s) address", address)
	}

	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, BasePath) {
		path += BasePath
	}

	parsed.Path = path

	return parsed.String(), nil
}

// WithCredential is the client, calling as an agent.
func (c *Client) WithCredential(credential string) *Client {
	copied := *c
	copied.credential = credential

	return &copied
}

// BaseURL is where this client calls.
func (c *Client) BaseURL() string {
	return c.base
}

// Register swaps an enrolment token for an identity and a credential.
func (c *Client) Register(ctx context.Context, token string, registration Registration) (Registered, error) {
	var registered Registered

	_, err := c.call(ctx, "register", EnrolmentHeader, token, registration, &registered)

	return registered, err
}

// Heartbeat says the agent is alive.
func (c *Client) Heartbeat(ctx context.Context, heartbeat Heartbeat) (Answer, error) {
	return c.call(ctx, "heartbeat", AgentHeader, c.credential, heartbeat, nil)
}

// Metrics sends a batch.
func (c *Client) Metrics(ctx context.Context, batch Batch) (Answer, error) {
	return c.call(ctx, "metrics", AgentHeader, c.credential, batch, nil)
}

// Inventory sends what the machine is and what runs on it. A platform older
// than the call answers 404, which Rejected() reports.
func (c *Client) Inventory(ctx context.Context, inventory Inventory) (Answer, error) {
	return c.call(ctx, "inventory", AgentHeader, c.credential, inventory, nil)
}

func (c *Client) call(ctx context.Context, name, header, secret string, body any, into any) (Answer, error) {
	if secret == "" {
		return Answer{}, errors.New("no credential: enrol this agent first")
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return Answer{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+name, bytes.NewReader(payload))
	if err != nil {
		return Answer{}, err
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set(header, secret)

	response, err := c.http.Do(request)
	if err != nil {
		return Answer{}, err
	}
	defer response.Body.Close()

	reply, err := io.ReadAll(io.LimitReader(response.Body, replyMax))
	if err != nil {
		return Answer{}, err
	}

	if response.StatusCode >= 300 {
		return Answer{}, refusal(response, reply)
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(reply, &envelope); err != nil || len(envelope.Data) == 0 {
		return Answer{}, fmt.Errorf("the platform's answer to %s is not the protocol's (HTTP %d)", name, response.StatusCode)
	}

	var answer Answer
	if err := json.Unmarshal(envelope.Data, &answer); err != nil {
		return Answer{}, err
	}

	if into != nil {
		if err := json.Unmarshal(envelope.Data, into); err != nil {
			return Answer{}, err
		}
	}

	return answer, nil
}

func refusal(response *http.Response, reply []byte) error {
	if response.StatusCode < 400 {
		return &Error{Status: response.StatusCode, Message: "redirected to " + response.Header.Get("Location") + "; use that address"}
	}

	var envelope struct {
		Error struct {
			Title   string            `json:"title"`
			Message string            `json:"message"`
			Fields  map[string]string `json:"fields"`
		} `json:"error"`
	}

	refused := &Error{Status: response.StatusCode, Title: http.StatusText(response.StatusCode)}

	if json.Unmarshal(reply, &envelope) == nil {
		if envelope.Error.Title != "" {
			refused.Title = envelope.Error.Title
		}

		refused.Message = envelope.Error.Message
		refused.Fields = envelope.Error.Fields
	}

	return refused
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
