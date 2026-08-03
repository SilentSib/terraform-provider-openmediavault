package omvclient

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestApplyChangesUsesDeployTimeoutNotRequestTimeout pins the exact fix
// for the reported bug: a slow Config.applyChanges (which OMV runs as a
// real Salt deployment in a separate daemon, not a quick database write)
// must be bounded by DeployTimeout, not the much shorter Timeout used for
// ordinary calls like get/set/delete/login. Simulates a server that
// responds slower than a short Timeout but faster than a long
// DeployTimeout, and confirms ApplyChanges succeeds while a plain Call
// hitting the same delay does not.
func TestApplyChangesUsesDeployTimeoutNotRequestTimeout(t *testing.T) {
	const serverDelay = 150 * time.Millisecond

	srv := newTestServer(t, func(t *testing.T, req rpcRequest, w http.ResponseWriter) {
		switch {
		case req.Service == "Config" && req.Method == "applyChanges":
			time.Sleep(serverDelay)
			_, _ = w.Write([]byte(`{"response": ["certificates"], "error": null}`))
		case req.Service == "System" && req.Method == "getInformation":
			time.Sleep(serverDelay)
			_, _ = w.Write([]byte(`{"response": {"version": "8.5.5-1"}, "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	defer srv.Close()

	c, err := New(Config{
		Host:          "127.0.0.1",
		Username:      "admin",
		Password:      "secret",
		Scheme:        "http",
		Timeout:       50 * time.Millisecond, // shorter than serverDelay
		DeployTimeout: 2 * time.Second,       // longer than serverDelay
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.baseURL = srv.URL + "/rpc.php"

	// ApplyChanges should succeed: it's bounded by the generous
	// DeployTimeout, not the short Timeout.
	if _, err := c.ApplyChanges(context.Background(), []string{"certificates"}, false); err != nil {
		t.Fatalf("ApplyChanges should have succeeded under DeployTimeout, got: %v", err)
	}

	// An ordinary Call (e.g. System.getInformation, standing in for any
	// get/set/delete call) hitting the same server-side delay SHOULD time
	// out under the short Timeout, confirming the two are genuinely
	// independent and this isn't accidentally passing because Timeout is
	// unused.
	err = c.Call(context.Background(), "System", "getInformation", nil, nil)
	if err == nil {
		t.Fatal("expected an ordinary Call to time out under the short request Timeout, but it succeeded")
	}
}

func TestClientDefaultTimeouts(t *testing.T) {
	c, err := New(Config{Host: "nas.local", Username: "admin"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.cfg.Timeout != 60*time.Second {
		t.Errorf("expected default Timeout of 60s, got %s", c.cfg.Timeout)
	}
	if c.cfg.DeployTimeout != 5*time.Minute {
		t.Errorf("expected default DeployTimeout of 5m, got %s", c.cfg.DeployTimeout)
	}
}
