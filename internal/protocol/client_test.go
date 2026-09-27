package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/laikait/lip-agent/internal/collect"
)

func TestTheAddressMustBeHTTPSUnlessItIsThisMachine(t *testing.T) {
	for address, want := range map[string]string{
		"https://platform.example":                   "https://platform.example/api/agent/v1",
		"https://platform.example/lip/api/agent/v1/": "https://platform.example/lip/api/agent/v1",
		"http://localhost/lip/api/agent/v1":          "http://localhost/lip/api/agent/v1",
		"http://127.0.0.1:8080":                      "http://127.0.0.1:8080/api/agent/v1",
	} {
		got, err := Base(address)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v", address, got, err)
		}
	}

	for _, address := range []string{"http://platform.example", "ftp://platform.example", "platform.example", "https://user:pass@platform.example", "https://platform.example/?token=x"} {
		if _, err := Base(address); err == nil {
			t.Errorf("%s was accepted", address)
		}
	}
}

func TestTheCredentialTravelsInItsHeaderOnly(t *testing.T) {
	var seen *http.Request
	var body map[string]any

	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"data":{"accepted":true,"heartbeatSeconds":60,"metricsSeconds":120,"protocol":1}}`)
	}))
	defer platform.Close()

	client, err := NewClient(platform.URL, "", "laika-agent/test")
	if err != nil {
		t.Fatal(err)
	}

	answer, err := client.WithCredential("lia_secret").Metrics(context.Background(), Batch{BatchID: "batch-0001", Samples: []collect.Sample{{SampledAt: "2026-09-27T10:00:00Z", CPU: 1}}})
	if err != nil {
		t.Fatal(err)
	}

	if seen.URL.Path != "/api/agent/v1/metrics" || strings.Contains(seen.URL.String(), "lia_") {
		t.Fatalf("called %s", seen.URL)
	}

	if seen.Header.Get(AgentHeader) != "lia_secret" || seen.Header.Get("Content-Type") != "application/json" || seen.Header.Get("User-Agent") != "laika-agent/test" {
		t.Fatalf("headers %v", seen.Header)
	}

	if !answer.Accepted || answer.MetricsSeconds != 120 {
		t.Fatalf("%+v", answer)
	}

	if body["batchId"] != "batch-0001" || body["services"] != nil {
		t.Fatalf("body %v: an empty service list is left out", body)
	}
}

func TestRefusalsAreTold(t *testing.T) {
	status := http.StatusUnprocessableEntity
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"status":422,"title":"Unprocessable","message":"Check the batch.","fields":{"samples.0.cpu":"Too high."}}}`)
	}))
	defer platform.Close()

	client, _ := NewClient(platform.URL, "", "test")
	client = client.WithCredential("lia_x")

	_, err := client.Heartbeat(context.Background(), Heartbeat{})
	if !Rejected(err) || Unauthorized(err) || !strings.Contains(err.Error(), "samples.0.cpu: Too high.") {
		t.Fatalf("%v", err)
	}

	status = http.StatusUnauthorized
	if _, err := client.Heartbeat(context.Background(), Heartbeat{}); !Unauthorized(err) || Rejected(err) {
		t.Fatalf("%v", err)
	}

	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		status = code
		if _, err := client.Heartbeat(context.Background(), Heartbeat{}); err == nil || Rejected(err) {
			t.Fatalf("%d is worth retrying: %v", code, err)
		}
	}
}

func TestARedirectIsNeverFollowed(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed, carrying %q", r.Header.Get(AgentHeader))
	}))
	defer elsewhere.Close()

	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer platform.Close()

	client, _ := NewClient(platform.URL, "", "test")

	_, err := client.WithCredential("lia_secret").Heartbeat(context.Background(), Heartbeat{})
	if err == nil || !strings.Contains(err.Error(), "redirected to") {
		t.Fatalf("%v", err)
	}
}

func TestNoCredentialNoCall(t *testing.T) {
	client, _ := NewClient("https://platform.example", "", "test")

	if _, err := client.Heartbeat(context.Background(), Heartbeat{}); err == nil {
		t.Fatal("called without a credential")
	}
}
