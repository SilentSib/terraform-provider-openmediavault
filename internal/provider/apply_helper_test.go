package provider

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// TestApplyOrHandleApplyFailureDistinguishesTimeoutFromRealFailure pins
// the exact fix for the reported production incident: a client-side
// timeout waiting for Config.applyChanges's response (which can happen
// legitimately on slow hardware, since the deploy runs in a separate,
// already-running daemon that keeps going regardless of whether this
// client is still listening) must produce a distinctly different,
// less alarming diagnostic than an actual RPC-level failure from OMV.
func TestApplyOrHandleApplyFailureDistinguishesTimeoutFromRealFailure(t *testing.T) {
	t.Run("client-side timeout: distinct, non-alarming diagnostic", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Service string `json:"service"`
				Method  string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Service == "session" && req.Method == "login" {
				_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated"}, "error": null}`))
				return
			}
			// Config.applyChanges: sleep longer than the client's
			// DeployTimeout below, forcing a genuine context deadline.
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"response": ["certificates"], "error": null}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		client := mustClientForHostPortWithTimeouts(t, srv, 5*time.Second, 20*time.Millisecond)

		var diags diag.Diagnostics
		applyOrHandleApplyFailure(context.Background(), client, false, []string{"certificates"}, &diags)

		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		got := diags[0].Summary()
		want := "Configuration Written, but Confirming the Deploy Timed Out (Deploy Likely Still Succeeded)"
		if got != want {
			t.Errorf("unexpected diagnostic summary for a client-side timeout: got %q, want %q", got, want)
		}
	})

	t.Run("real RPC error from OMV: the normal, unambiguous diagnostic", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Service string `json:"service"`
				Method  string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Service == "session" && req.Method == "login" {
				_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated"}, "error": null}`))
				return
			}
			_, _ = w.Write([]byte(`{"response": null, "error": {"code": 9001, "message": "salt state failed"}}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		client := mustClientForHostPort(t, srv)

		var diags diag.Diagnostics
		applyOrHandleApplyFailure(context.Background(), client, false, []string{"certificates"}, &diags)

		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		got := diags[0].Summary()
		want := "Configuration Written, but Deploying It Failed"
		if got != want {
			t.Errorf("unexpected diagnostic summary for a real RPC error: got %q, want %q", got, want)
		}
	})
}

func TestIsLikelyClientSideTimeout(t *testing.T) {
	if !isLikelyClientSideTimeout(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded should be detected as a client-side timeout")
	}
	if isLikelyClientSideTimeout(nil) {
		t.Error("nil should not be detected as a timeout")
	}
	// A plain *rpcError-shaped failure (simulated here since rpcError is
	// unexported in another package) should not match: use any ordinary
	// error without a Timeout() method.
	if isLikelyClientSideTimeout(errPlain("salt state failed")) {
		t.Error("an ordinary error should not be detected as a timeout")
	}
}

type errPlain string

func (e errPlain) Error() string { return string(e) }

// mustClientForHostPortWithTimeouts is mustClientForHostPort (defined in
// shared_folder_apply_test.go) with explicit control over both timeouts,
// needed here to force a real client-side deadline in a test.
func mustClientForHostPortWithTimeouts(t *testing.T, srv *httptest.Server, timeout, deployTimeout time.Duration) *omvclient.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL %q: %v", srv.URL, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("failed to split host/port from %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("failed to parse port %q: %v", portStr, err)
	}
	c, err := omvclient.New(omvclient.Config{
		Host:          host,
		Port:          port,
		Username:      "admin",
		Password:      "x",
		Scheme:        "http",
		Timeout:       timeout,
		DeployTimeout: deployTimeout,
	})
	if err != nil {
		t.Fatalf("omvclient.New: %v", err)
	}
	return c
}
