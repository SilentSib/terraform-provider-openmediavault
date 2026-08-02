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

// testShareMgmtGetServer returns a fake OMV server that answers
// session.login and ShareMgmt.get (the only calls SharedFolderResource.Read
// makes), with a get() response that -- like the real API -- never
// includes "mode".
func testShareMgmtGetServer(t *testing.T) *httptest.Server {
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
			_, _ = w.Write([]byte(`{"response": {"username": "admin", "status": "authenticated"}, "error": null}`))
		case req.Service == "ShareMgmt" && req.Method == "get":
			// Realistic get() response: no "mode" key at all.
			_, _ = w.Write([]byte(`{"response": {
				"uuid": "de6fde5b-f38f-4c4b-885e-6cdc48c1c64c",
				"name": "Syslog",
				"mntentref": "fs-uuid",
				"reldirpath": "Syslog/",
				"comment": "",
				"mountpoint": "/srv/dev-disk-by-uuid-fs-uuid"
			}, "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	return httptest.NewServer(mux)
}

// TestSharedFolderReadPostImportModeFallback reproduces the exact bug
// reported in production: right after `terraform import`, state has no
// "mode" value at all (ImportStatePassthroughID only sets id), and since
// ShareMgmt.get() never returns "mode", a naive Read() would leave it null
// forever -- causing `terraform plan` to show a spurious "+ mode" diff on
// every single plan afterward, even when the configured value already
// matches reality. Read() must fall back to the schema default in this
// specific (state has no value yet) case.
func TestSharedFolderReadPostImportModeFallback(t *testing.T) {
	srv := testShareMgmtGetServer(t)
	defer srv.Close()

	r := &SharedFolderResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	t.Run("mode null in state (post-import): falls back to the default", func(t *testing.T) {
		importedState := sharedFolderResourceModel{
			ID:           types.StringValue("de6fde5b-f38f-4c4b-885e-6cdc48c1c64c"),
			Name:         types.StringValue("Syslog"),
			MountPointID: types.StringValue("fs-uuid"),
			RelativePath: types.StringValue("Syslog/"),
			Comment:      types.StringValue(""),
			Mode:         types.StringNull(), // exactly what ImportStatePassthroughID leaves it as
		}
		req, resp := buildReadCall(t, sch, &importedState)

		r.Read(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("Read failed: %v", resp.Diagnostics)
		}

		var out sharedFolderResourceModel
		resp.State.Get(context.Background(), &out)
		if out.Mode.IsNull() {
			t.Fatal("Read left Mode null after a fresh import -- this is the reported bug: every " +
				"subsequent `terraform plan` would show a spurious 'mode will be set' diff")
		}
		if out.Mode.ValueString() != sharedFolderDefaultMode {
			t.Errorf("expected the post-import Mode fallback to be %q, got %q", sharedFolderDefaultMode, out.Mode.ValueString())
		}
	})

	t.Run("mode already set in state: left untouched by a normal refresh", func(t *testing.T) {
		existingState := sharedFolderResourceModel{
			ID:           types.StringValue("de6fde5b-f38f-4c4b-885e-6cdc48c1c64c"),
			Name:         types.StringValue("Syslog"),
			MountPointID: types.StringValue("fs-uuid"),
			RelativePath: types.StringValue("Syslog/"),
			Comment:      types.StringValue(""),
			Mode:         types.StringValue("750"), // e.g. set by a prior Create/Update
		}
		req, resp := buildReadCall(t, sch, &existingState)

		r.Read(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("Read failed: %v", resp.Diagnostics)
		}

		var out sharedFolderResourceModel
		resp.State.Get(context.Background(), &out)
		if out.Mode.ValueString() != "750" {
			t.Errorf("Read must not overwrite an already-known Mode value: got %q, want %q", out.Mode.ValueString(), "750")
		}
	})
}

// schemaOf extracts the resource's schema via its Schema() method.
func schemaOf(t *testing.T, r resource.Resource) resource.SchemaResponse {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Schema() failed: %v", resp.Diagnostics)
	}
	return resp
}

// buildReadCall constructs a resource.ReadRequest/ReadResponse pair with
// State populated from model, for directly exercising a resource's Read
// method in tests without a real Terraform CLI.
func buildReadCall(t *testing.T, schemaResp resource.SchemaResponse, model interface{}) (resource.ReadRequest, resource.ReadResponse) {
	t.Helper()
	state := tfsdk.State{Schema: schemaResp.Schema}
	diags := state.Set(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("failed to build initial state: %v", diags)
	}
	return resource.ReadRequest{State: state}, resource.ReadResponse{State: state}
}
