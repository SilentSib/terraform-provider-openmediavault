package omvclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			_, _ = w.Write([]byte(`{"response": true, "error": null}`))
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
		_, _ = w.Write([]byte(`{"response": {"version": "8.5.5-1", "hostname": "nas"}, "error": null}`))
	})
	defer srv.Close()

	c := testClient(t, srv)
	info, err := c.GetSystemInformation(context.Background())
	if err != nil {
		t.Fatalf("GetSystemInformation failed: %v", err)
	}
	if info.Version != "8.5.5-1" || info.Hostname != "nas" {
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
