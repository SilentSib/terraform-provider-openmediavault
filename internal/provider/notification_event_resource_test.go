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

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// notificationTestServer scripts a fake Notification service.
// getListResponse is returned verbatim from getList(); setFn is called
// for every set() with the decoded request object and should return
// what OMV would respond with.
func notificationTestServer(t *testing.T, getListResponse string, setFn func(t *testing.T, obj notificationEventRPCObject) notificationEventRPCObject) *httptest.Server {
	t.Helper()
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
		case req.Service == "Notification" && req.Method == "getList":
			_, _ = w.Write([]byte(`{"response": ` + getListResponse + `, "error": null}`))
		case req.Service == "Notification" && req.Method == "set":
			var obj notificationEventRPCObject
			b, _ := json.Marshal(req.Params)
			if err := json.Unmarshal(b, &obj); err != nil {
				t.Fatalf("failed to decode set() params: %v", err)
			}
			result := setFn(t, obj)
			b2, _ := json.Marshal(map[string]interface{}{"response": result, "error": nil})
			_, _ = w.Write(b2)
		case req.Service == "Notification" && req.Method == "get":
			t.Fatalf("unexpected get() call in this test")
		case req.Service == "Config" && req.Method == "applyChanges":
			_, _ = w.Write([]byte(`{"response": ["smartmontools"], "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	return httptest.NewServer(mux)
}

func TestNotificationEventCreateNew(t *testing.T) {
	// getList() shows smartmontools as never-configured (uuid == "").
	getListResp := `[{"uuid": "", "id": "smartmontools", "enable": false, "title": "S.M.A.R.T.", "type": "email"}]`

	var sentUUID string
	srv := notificationTestServer(t, getListResp, func(t *testing.T, obj notificationEventRPCObject) notificationEventRPCObject {
		sentUUID = obj.UUID
		return notificationEventRPCObject{UUID: "new-real-uuid", ID: obj.ID, Enable: obj.Enable}
	})
	defer srv.Close()

	r := &NotificationEventResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := notificationEventResourceModel{
		EventID: types.StringValue("smartmontools"),
		Enabled: types.BoolValue(true),
	}
	planTF := tfsdk.Plan{Schema: sch.Schema}
	if diags := planTF.Set(context.Background(), &plan); diags.HasError() {
		t.Fatalf("failed to build plan: %v", diags)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: planTF}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create failed: %v", createResp.Diagnostics)
	}

	if sentUUID != omvclient.NewObjectUUID {
		t.Errorf("expected the new-object sentinel to be sent for a never-configured event, got %q", sentUUID)
	}

	var created notificationEventResourceModel
	createResp.State.Get(context.Background(), &created)
	if created.ID.ValueString() != "new-real-uuid" {
		t.Errorf("unexpected id after Create: %q", created.ID.ValueString())
	}
	if !created.Enabled.ValueBool() {
		t.Error("expected enabled=true after Create")
	}
}

func TestNotificationEventCreateAdoptsExisting(t *testing.T) {
	// getList() shows smartmontools ALREADY has a persisted object.
	getListResp := `[{"uuid": "already-exists-uuid", "id": "smartmontools", "enable": false, "title": "S.M.A.R.T.", "type": "email"}]`

	var sentUUID string
	srv := notificationTestServer(t, getListResp, func(t *testing.T, obj notificationEventRPCObject) notificationEventRPCObject {
		sentUUID = obj.UUID
		return notificationEventRPCObject{UUID: obj.UUID, ID: obj.ID, Enable: obj.Enable}
	})
	defer srv.Close()

	r := &NotificationEventResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := notificationEventResourceModel{
		EventID: types.StringValue("smartmontools"),
		Enabled: types.BoolValue(true),
	}
	planTF := tfsdk.Plan{Schema: sch.Schema}
	if diags := planTF.Set(context.Background(), &plan); diags.HasError() {
		t.Fatalf("failed to build plan: %v", diags)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: planTF}, &createResp)
	if createResp.Diagnostics.HasError() {
		t.Fatalf("Create failed: %v", createResp.Diagnostics)
	}

	// Critical assertion: Create must NOT send the sentinel here -- it
	// must adopt (reuse) the already-existing UUID, or a duplicate config
	// object would be created for the same event_id.
	if sentUUID != "already-exists-uuid" {
		t.Errorf("expected Create to adopt the existing uuid %q, but it sent %q", "already-exists-uuid", sentUUID)
	}

	var created notificationEventResourceModel
	createResp.State.Get(context.Background(), &created)
	if created.ID.ValueString() != "already-exists-uuid" {
		t.Errorf("unexpected id after Create: %q", created.ID.ValueString())
	}
}

func TestNotificationEventCreateUnrecognizedEventID(t *testing.T) {
	getListResp := `[{"uuid": "", "id": "smartmontools", "enable": false, "title": "S.M.A.R.T.", "type": "email"}]`
	srv := notificationTestServer(t, getListResp, func(t *testing.T, obj notificationEventRPCObject) notificationEventRPCObject {
		t.Fatal("set() should not be called for an unrecognized event_id")
		return notificationEventRPCObject{}
	})
	defer srv.Close()

	r := &NotificationEventResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := notificationEventResourceModel{
		EventID: types.StringValue("totally_made_up_event"),
		Enabled: types.BoolValue(true),
	}
	planTF := tfsdk.Plan{Schema: sch.Schema}
	if diags := planTF.Set(context.Background(), &plan); diags.HasError() {
		t.Fatalf("failed to build plan: %v", diags)
	}

	createResp := resource.CreateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: planTF}, &createResp)
	if !createResp.Diagnostics.HasError() {
		t.Fatal("expected an error for an unrecognized event_id")
	}
}

func TestNotificationEventImportStateNothingToImport(t *testing.T) {
	getListResp := `[{"uuid": "", "id": "smartmontools", "enable": false, "title": "S.M.A.R.T.", "type": "email"}]`
	srv := notificationTestServer(t, getListResp, func(t *testing.T, obj notificationEventRPCObject) notificationEventRPCObject {
		t.Fatal("set() should not be called during ImportState")
		return notificationEventRPCObject{}
	})
	defer srv.Close()

	r := &NotificationEventResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "smartmontools"}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error importing an event_id with no persisted object yet")
	}
}

func TestNotificationEventImportStateSuccess(t *testing.T) {
	getListResp := `[{"uuid": "existing-uuid", "id": "apt", "enable": true, "title": "APT", "type": "email"}]`
	srv := notificationTestServer(t, getListResp, func(t *testing.T, obj notificationEventRPCObject) notificationEventRPCObject {
		t.Fatal("set() should not be called during ImportState")
		return notificationEventRPCObject{}
	})
	defer srv.Close()

	r := &NotificationEventResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "apt"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("ImportState failed: %v", resp.Diagnostics)
	}

	var imported notificationEventResourceModel
	resp.State.Get(context.Background(), &imported)
	if imported.ID.ValueString() != "existing-uuid" {
		t.Errorf("unexpected id: %q", imported.ID.ValueString())
	}
	if imported.EventID.ValueString() != "apt" {
		t.Errorf("unexpected event_id: %q", imported.EventID.ValueString())
	}
	if !imported.Enabled.ValueBool() {
		t.Error("expected enabled=true")
	}
}

func TestNotificationEventDeleteResetsToDisabled(t *testing.T) {
	var lastSetParams notificationEventRPCObject
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
		case req.Service == "Notification" && req.Method == "set":
			b, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(b, &lastSetParams)
			_, _ = w.Write([]byte(`{"response": null, "error": null}`))
		case req.Service == "Config" && req.Method == "applyChanges":
			_, _ = w.Write([]byte(`{"response": ["smartmontools"], "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &NotificationEventResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	state := notificationEventResourceModel{
		ID:      types.StringValue("some-uuid"),
		EventID: types.StringValue("smartmontools"),
		Enabled: types.BoolValue(true), // currently enabled
	}
	stateTF := tfsdk.State{Schema: sch.Schema}
	if diags := stateTF.Set(context.Background(), &state); diags.HasError() {
		t.Fatalf("failed to build state: %v", diags)
	}

	resp := resource.DeleteResponse{State: stateTF}
	r.Delete(context.Background(), resource.DeleteRequest{State: stateTF}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete failed: %v", resp.Diagnostics)
	}

	if lastSetParams.Enable {
		t.Error("expected Delete to send enable=false, resetting the event to disabled")
	}
	if lastSetParams.UUID != "some-uuid" {
		t.Errorf("expected Delete to use the existing uuid, got %q", lastSetParams.UUID)
	}
}
