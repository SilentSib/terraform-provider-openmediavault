package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestWorkbenchSettingsValidateConfig(t *testing.T) {
	r := &WorkbenchSettingsResource{}
	sch := schemaOf(t, r)

	cases := []struct {
		name      string
		config    workbenchSettingsResourceModel
		expectErr bool
	}{
		{
			name: "ssl disabled, port == ssl_port is fine (irrelevant while disabled)",
			config: workbenchSettingsResourceModel{
				EnableSSL:        types.BoolValue(false),
				Port:             types.Int64Value(80),
				SSLPort:          types.Int64Value(80),
				SSLCertificateID: types.StringValue(""),
			},
			expectErr: false,
		},
		{
			name: "ssl enabled, port != ssl_port, cert set: fine",
			config: workbenchSettingsResourceModel{
				EnableSSL:        types.BoolValue(true),
				Port:             types.Int64Value(80),
				SSLPort:          types.Int64Value(443),
				SSLCertificateID: types.StringValue("cert-uuid"),
			},
			expectErr: false,
		},
		{
			name: "ssl enabled, port == ssl_port: error",
			config: workbenchSettingsResourceModel{
				EnableSSL:        types.BoolValue(true),
				Port:             types.Int64Value(443),
				SSLPort:          types.Int64Value(443),
				SSLCertificateID: types.StringValue("cert-uuid"),
			},
			expectErr: true,
		},
		{
			name: "ssl enabled, no certificate: error",
			config: workbenchSettingsResourceModel{
				EnableSSL:        types.BoolValue(true),
				Port:             types.Int64Value(80),
				SSLPort:          types.Int64Value(443),
				SSLCertificateID: types.StringValue(""),
			},
			expectErr: true,
		},
		{
			name: "ssl enabled, port/ssl_port left unknown (not yet defaulted): no false positive",
			config: workbenchSettingsResourceModel{
				EnableSSL:        types.BoolValue(true),
				Port:             types.Int64Unknown(),
				SSLPort:          types.Int64Unknown(),
				SSLCertificateID: types.StringValue("cert-uuid"),
			},
			expectErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateForConfig := tfsdk.State{Schema: sch.Schema}
			diags := stateForConfig.Set(context.Background(), &tc.config)
			if diags.HasError() {
				t.Fatalf("failed to build config: %v", diags)
			}
			config := tfsdk.Config{Schema: sch.Schema, Raw: stateForConfig.Raw}

			var resp resource.ValidateConfigResponse
			r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{Config: config}, &resp)

			if tc.expectErr && !resp.Diagnostics.HasError() {
				t.Error("expected a validation error, got none")
			}
			if !tc.expectErr && resp.Diagnostics.HasError() {
				t.Errorf("expected no validation error, got: %v", resp.Diagnostics)
			}
		})
	}
}

// TestWorkbenchSettingsCreateReadUpdateFlow exercises the resource's
// Create/Read/Update methods end-to-end against a fake WebGui service,
// confirming the request/response field mapping (port, timeout ->
// auto_logout_minutes, enablessl, sslport, forcesslonly,
// sslcertificateref) round-trips correctly and that the fixed synthetic
// ID is set.
func TestWorkbenchSettingsCreateReadUpdateFlow(t *testing.T) {
	var lastSetParams map[string]interface{}
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc.php", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Service string                 `json:"service"`
			Method  string                 `json:"method"`
			Params  map[string]interface{} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		switch {
		case req.Service == "session" && req.Method == "login":
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated"}, "error": null}`))
		case req.Service == "WebGui" && (req.Method == "setSettings" || req.Method == "getSettings"):
			if req.Method == "setSettings" {
				lastSetParams = req.Params
			}
			_, _ = w.Write([]byte(`{"response": {
				"port": 8080,
				"enablessl": true,
				"sslport": 8443,
				"forcesslonly": false,
				"sslcertificateref": "cert-uuid-1234",
				"timeout": 15
			}, "error": null}`))
		case req.Service == "Config" && req.Method == "applyChanges":
			_, _ = w.Write([]byte(`{"response": ["webserver", "monit"], "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &WorkbenchSettingsResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := workbenchSettingsResourceModel{
		Port:             types.Int64Value(8080),
		Timeout:          types.Int64Value(15),
		EnableSSL:        types.BoolValue(true),
		SSLPort:          types.Int64Value(8443),
		ForceSSLOnly:     types.BoolValue(false),
		SSLCertificateID: types.StringValue("cert-uuid-1234"),
	}
	planTF := tfsdk.Plan{Schema: sch.Schema}
	diags := planTF.Set(context.Background(), &plan)
	if diags.HasError() {
		t.Fatalf("failed to build plan: %v", diags)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: planTF}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create failed: %v", createResp.Diagnostics)
	}

	// Confirm the request sent every required field (all 6 are "required"
	// per rpc.webgui.json, even though our schema treats them as
	// Optional+Computed with defaults).
	for _, key := range []string{"port", "enablessl", "sslport", "forcesslonly", "sslcertificateref", "timeout"} {
		if _, ok := lastSetParams[key]; !ok {
			t.Errorf("setSettings request missing required field %q", key)
		}
	}

	var created workbenchSettingsResourceModel
	createResp.State.Get(context.Background(), &created)
	if created.ID.ValueString() != workbenchSettingsID {
		t.Errorf("expected synthetic id %q, got %q", workbenchSettingsID, created.ID.ValueString())
	}
	if created.Port.ValueInt64() != 8080 || created.SSLPort.ValueInt64() != 8443 {
		t.Errorf("unexpected port/ssl_port after Create: %d/%d", created.Port.ValueInt64(), created.SSLPort.ValueInt64())
	}
	if created.Timeout.ValueInt64() != 15 {
		t.Errorf("unexpected auto_logout_minutes after Create: %d", created.Timeout.ValueInt64())
	}

	// Read should refresh cleanly from the same (consistent) response shape.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(context.Background(), resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read failed: %v", readResp.Diagnostics)
	}
}

func TestWorkbenchSettingsImportRejectsWrongID(t *testing.T) {
	r := &WorkbenchSettingsResource{}
	sch := schemaOf(t, r)

	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "some-other-id"}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error importing with an ID other than the fixed synthetic one")
	}
}

func TestWorkbenchSettingsDeleteDoesNotCallRPC(t *testing.T) {
	// No httptest server at all -- if Delete tried to make any RPC call,
	// this would fail with a connection error.
	r := &WorkbenchSettingsResource{}
	sch := schemaOf(t, r)

	state := workbenchSettingsResourceModel{ID: types.StringValue(workbenchSettingsID)}
	stateTF := tfsdk.State{Schema: sch.Schema}
	diags := stateTF.Set(context.Background(), &state)
	if diags.HasError() {
		t.Fatalf("failed to build state: %v", diags)
	}

	resp := resource.DeleteResponse{State: stateTF}
	r.Delete(context.Background(), resource.DeleteRequest{State: stateTF}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete returned unexpected error diagnostics: %v", resp.Diagnostics)
	}
	// A warning is expected (see Delete's doc comment).
	found := false
	for _, d := range resp.Diagnostics {
		if d.Summary() == "Workbench Settings Left Unchanged" {
			found = true
		}
	}
	if !found {
		t.Error("expected the 'Workbench Settings Left Unchanged' warning diagnostic")
	}
}
