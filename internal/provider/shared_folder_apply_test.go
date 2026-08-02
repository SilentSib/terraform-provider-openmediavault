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

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// testOMVServer scripts a fake rpc.php: session/login always succeeds,
// Config.applyChanges and Config.revertChanges behave per the applyOK /
// revertOK flags, and everything else 500s (unused in these tests).
func testOMVServer(t *testing.T, applyOK, revertOK bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Service string `json:"service"`
			Method  string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		switch {
		case req.Service == "session" && req.Method == "login":
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated", "sessionid": "test-session"}, "error": null}`))
		case req.Service == "Config" && req.Method == "applyChanges":
			if applyOK {
				_, _ = w.Write([]byte(`{"response": ["sharedfolders"], "error": null}`))
			} else {
				_, _ = w.Write([]byte(`{"response": null, "error": {"code": 5002, "message": "deploy failed"}}`))
			}
		case req.Service == "Config" && req.Method == "revertChanges":
			if revertOK {
				_, _ = w.Write([]byte(`{"response": null, "error": null}`))
			} else {
				_, _ = w.Write([]byte(`{"response": null, "error": {"code": 5003, "message": "revert failed"}}`))
			}
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	return httptest.NewServer(mux)
}

func mustClientForHostPort(t *testing.T, srv *httptest.Server) *omvclient.Client {
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
		Host:     host,
		Port:     port,
		Username: "admin",
		Password: "x",
		Scheme:   "http",
	})
	if err != nil {
		t.Fatalf("omvclient.New: %v", err)
	}
	return c
}

func TestApplyOrHandleApplyFailure(t *testing.T) {
	t.Run("apply succeeds: no diagnostics", func(t *testing.T) {
		srv := testOMVServer(t, true, true)
		defer srv.Close()

		r := &SharedFolderResource{client: mustClientForHostPort(t, srv), revertOnApplyFailure: false}
		var diags diag.Diagnostics
		r.applyOrHandleApplyFailure(context.Background(), &diags)
		if diags.HasError() {
			t.Fatalf("expected no error diagnostics, got: %v", diags)
		}
	})

	t.Run("apply fails, revert disabled: blocking error, no auto-revert", func(t *testing.T) {
		srv := testOMVServer(t, false, true)
		defer srv.Close()

		r := &SharedFolderResource{client: mustClientForHostPort(t, srv), revertOnApplyFailure: false}
		var diags diag.Diagnostics
		r.applyOrHandleApplyFailure(context.Background(), &diags)
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		if got := diags[0].Summary(); got != "Configuration Written, but Deploying It Failed" {
			t.Errorf("unexpected diagnostic summary: %q", got)
		}
	})

	t.Run("apply fails, revert enabled and succeeds: error explains the revert", func(t *testing.T) {
		srv := testOMVServer(t, false, true)
		defer srv.Close()

		r := &SharedFolderResource{client: mustClientForHostPort(t, srv), revertOnApplyFailure: true}
		var diags diag.Diagnostics
		r.applyOrHandleApplyFailure(context.Background(), &diags)
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		if got := diags[0].Summary(); got != "Failed to Apply Changes; Reverted All Pending Changes" {
			t.Errorf("unexpected diagnostic summary: %q", got)
		}
	})

	t.Run("apply fails, revert enabled but also fails: error explains both failures", func(t *testing.T) {
		srv := testOMVServer(t, false, false)
		defer srv.Close()

		r := &SharedFolderResource{client: mustClientForHostPort(t, srv), revertOnApplyFailure: true}
		var diags diag.Diagnostics
		r.applyOrHandleApplyFailure(context.Background(), &diags)
		if !diags.HasError() {
			t.Fatal("expected an error diagnostic")
		}
		if got := diags[0].Summary(); got != "Failed to Apply Changes, and the Automatic Revert Also Failed" {
			t.Errorf("unexpected diagnostic summary: %q", got)
		}
	})
}
