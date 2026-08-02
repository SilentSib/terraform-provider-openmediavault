package omvclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer wires up a minimal fake OMV rpc.php: "session"/"login"
// always succeeds, and handle is invoked for everything else so each test
// can script the specific behavior it needs.
func newTestServer(t *testing.T, handle func(t *testing.T, req rpcRequest, w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Service == "session" && req.Method == "login" {
			http.SetCookie(w, &http.Cookie{Name: "X-OPENMEDIAVAULT-LOGIN", Value: "1"})
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated", "sessionid": "test-session"}, "error": null}`))
			return
		}
		handle(t, req, w)
	})
	return httptest.NewServer(mux)
}

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{
		Host:     "127.0.0.1", // overridden by baseURL below
		Username: "admin",
		Password: "secret",
		Scheme:   "http",
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	c.baseURL = srv.URL + "/rpc.php"
	return c
}

func TestClientLoginAndCall(t *testing.T) {
	srv := newTestServer(t, func(t *testing.T, req rpcRequest, w http.ResponseWriter) {
		if req.Service != "System" || req.Method != "getInformation" {
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
		// A realistic full response: version includes a trailing release
		// codename, and memTotal is a JSON *string* (per system.inc's own
		// doc comment: "all numbers that might be > 4GiB are returned as
		// strings"). SystemInformation intentionally doesn't model
		// memTotal/cpuUtilization/etc, so this must decode cleanly despite
		// their presence -- this is the exact shape that used to crash
		// with "cannot unmarshal string into Go struct field ... memTotal
		// of type int64" when those fields were modeled with fixed types.
		_, _ = w.Write([]byte(`{"response": {
			"ts": 1735689600,
			"time": "Thu Jan  1 00:00:00 2026",
			"hostname": "nas",
			"version": "8.5.5-1 (Shaitung)",
			"cpuModelName": "Some CPU",
			"cpuUtilization": 3.2,
			"memTotal": "17179869184",
			"memFree": 1234567,
			"kernel": "Linux 6.1.0",
			"configDirty": false,
			"dirtyModules": []
		}, "error": null}`))
	})
	defer srv.Close()

	c := testClient(t, srv)
	info, err := c.GetSystemInformation(context.Background())
	if err != nil {
		t.Fatalf("GetSystemInformation failed: %v", err)
	}
	if info.Version != "8.5.5-1 (Shaitung)" || info.Hostname != "nas" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestCheckMinVersion(t *testing.T) {
	cases := []struct {
		name       string
		version    string
		min        int
		expectFail bool
	}{
		{"newer major passes", "8.5.5-1", 8, false},
		{"much newer major passes", "9.0.0-1", 8, false},
		{"older major fails", "7.9.9-1", 8, true},
		{"realistic version with codename passes", "8.5.5-1 (Shaitung)", 8, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, func(t *testing.T, req rpcRequest, w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"response": {"version": "` + tc.version + `"}, "error": null}`))
			})
			defer srv.Close()

			c := testClient(t, srv)
			err := c.CheckMinVersion(context.Background(), tc.min)
			if tc.expectFail && err == nil {
				t.Errorf("expected CheckMinVersion to fail for version %q >= %d", tc.version, tc.min)
			}
			if !tc.expectFail && err != nil {
				t.Errorf("expected CheckMinVersion to pass for version %q >= %d, got: %v", tc.version, tc.min, err)
			}
		})
	}

	t.Run("missing version field (non-admin account) gives a clear error", func(t *testing.T) {
		// System.getInformation only includes "version" (and most other
		// fields) when the caller has the administrator role -- a
		// non-admin account gets back just ts/time/hostname.
		srv := newTestServer(t, func(t *testing.T, req rpcRequest, w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"response": {"ts": 1735689600, "time": "now", "hostname": "nas"}, "error": null}`))
		})
		defer srv.Close()

		c := testClient(t, srv)
		err := c.CheckMinVersion(context.Background(), 8)
		if err == nil {
			t.Fatal("expected an error when the version field is absent")
		}
		if !strings.Contains(err.Error(), "administrator role") {
			t.Errorf("expected error to mention the administrator role requirement, got: %v", err)
		}
	})
}

func TestCallSurfacesRPCError(t *testing.T) {
	srv := newTestServer(t, func(t *testing.T, req rpcRequest, w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"response": null, "error": {"code": 5001, "message": "Object not found", "trace": "..."}}`))
	})
	defer srv.Close()

	c := testClient(t, srv)
	err := c.Call(context.Background(), "ShareMgmt", "get", map[string]string{"uuid": "does-not-exist"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var rpcErr *rpcError
	if !asRPCError(err, &rpcErr) {
		t.Fatalf("expected error to unwrap to *rpcError, got %T: %v", err, err)
	}
	if rpcErr.Code != 5001 {
		t.Errorf("unexpected error code: %d", rpcErr.Code)
	}
}

// asRPCError is a small helper since rpcError doesn't implement the
// standard errors.Unwrap chain (it's returned directly, not wrapped).
func asRPCError(err error, target **rpcError) bool {
	if e, ok := err.(*rpcError); ok {
		*target = e
		return true
	}
	return false
}

func TestApplyChangesAndRevertChanges(t *testing.T) {
	var lastApplyReq, lastRevertReq rpcRequest
	srv := newTestServer(t, func(t *testing.T, req rpcRequest, w http.ResponseWriter) {
		switch {
		case req.Service == "Config" && req.Method == "applyChanges":
			lastApplyReq = req
			_, _ = w.Write([]byte(`{"response": ["sharedfolders", "systemd"], "error": null}`))
		case req.Service == "Config" && req.Method == "revertChanges":
			lastRevertReq = req
			_, _ = w.Write([]byte(`{"response": null, "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	defer srv.Close()

	c := testClient(t, srv)
	applied, err := c.ApplyChanges(context.Background(), []string{"sharedfolders", "systemd"}, false)
	if err != nil {
		t.Fatalf("ApplyChanges failed: %v", err)
	}
	if len(applied) != 2 {
		t.Errorf("unexpected applied modules: %v", applied)
	}
	params, ok := lastApplyReq.Params.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected params type: %T", lastApplyReq.Params)
	}
	if params["force"] != false {
		t.Errorf("expected force=false, got %v", params["force"])
	}

	if err := c.RevertChanges(context.Background(), ""); err != nil {
		t.Fatalf("RevertChanges failed: %v", err)
	}
	if lastRevertReq.Service != "Config" || lastRevertReq.Method != "revertChanges" {
		t.Errorf("revertChanges was not called as expected")
	}
}

// TestLoginResponseShapes guards against the exact regression reported in
// production: Session.login's real response is an object describing
// status ("authenticated" or "challengeRequired"), never a bare boolean.
// An earlier version of this client assumed a bool response and failed
// every real login with "cannot unmarshal object into Go value of type
// bool".
func TestLoginResponseShapes(t *testing.T) {
	t.Run("authenticated succeeds", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated", "sessionid": "abc123"}, "error": null}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := testClient(t, srv)
		if err := c.Login(context.Background()); err != nil {
			t.Fatalf("Login failed: %v", err)
		}
	})

	t.Run("challengeRequired (MFA) returns a clear, non-panicking error", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "challengeRequired", "challenge": {"kind": "totp"}}, "error": null}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := testClient(t, srv)
		err := c.Login(context.Background())
		if err == nil {
			t.Fatal("expected an error for a challengeRequired login")
		}
		if !strings.Contains(err.Error(), "multi-factor") || !strings.Contains(err.Error(), "totp") {
			t.Errorf("expected error to mention MFA and the challenge kind, got: %v", err)
		}
	})

	t.Run("legacy bare-boolean response is a decode error, not a silent success", func(t *testing.T) {
		// This is what the server used to be (incorrectly) mocked as, and
		// what this client used to (incorrectly) expect. Confirms the
		// fixed client no longer accepts this shape.
		mux := http.NewServeMux()
		mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"response": true, "error": null}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := testClient(t, srv)
		if err := c.Login(context.Background()); err == nil {
			t.Fatal("expected an error decoding a bare boolean into the login response struct")
		}
	})
}
