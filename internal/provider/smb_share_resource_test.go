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

func TestTimeMachineMaxSizeRegexp(t *testing.T) {
	valid := []string{"", "0", "500", "500G", "2T", "1K", "999999M", "1P"}
	invalid := []string{"G", "500g", "500 G", "500GB", "-500G", "5.5G"}
	for _, s := range valid {
		if !timeMachineMaxSizeRegexp.MatchString(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	for _, s := range invalid {
		if timeMachineMaxSizeRegexp.MatchString(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

// TestSMBShareRPCObjectRoundTrip confirms toRPCObject/fromSMBShareRPCObject
// map every field correctly and consistently in both directions, with
// particular attention to the byte/day-unit fields and the fields that
// are absent from rpc.smb.setshare's schema but still settable (see this
// file's top doc comment).
func TestSMBShareRPCObjectRoundTrip(t *testing.T) {
	r := &SMBShareResource{}

	plan := smbShareResourceModel{
		Enabled:                 types.BoolValue(true),
		SharedFolderID:          types.StringValue("11111111-1111-1111-1111-111111111111"),
		Comment:                 types.StringValue("media share"),
		Guest:                   types.StringValue("allow"),
		ReadOnly:                types.BoolValue(false),
		Browseable:              types.BoolValue(true),
		RecycleBin:              types.BoolValue(true),
		RecycleBinMaxSizeBytes:  types.Int64Value(1048576),
		RecycleBinRetentionDays: types.Int64Value(30),
		HideDotFiles:            types.BoolValue(true),
		InheritACLs:             types.BoolValue(true),
		InheritPermissions:      types.BoolValue(false),
		EASupport:               types.BoolValue(true),
		StoreDOSAttributes:      types.BoolValue(false),
		HostsAllow:              types.StringValue("192.168.1.0/24"),
		HostsDeny:               types.StringValue(""),
		Audit:                   types.BoolValue(false),
		TimeMachine:             types.BoolValue(true),
		TimeMachineMaxSize:      types.StringValue("500G"),
		TransportEncryption:     types.BoolValue(true),
		FollowSymlinks:          types.BoolValue(true),
		WideLinks:               types.BoolValue(false),
		ExtraOptions:            types.StringValue("veto files = /._*/"),
	}

	obj := r.toRPCObject("22222222-2222-2222-2222-222222222222", &plan)

	if obj.UUID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("unexpected UUID: %s", obj.UUID)
	}
	if obj.SharedFolderRef != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("unexpected sharedfolderref: %s", obj.SharedFolderRef)
	}
	if obj.RecycleMaxSize != 1048576 || obj.RecycleMaxAge != 30 {
		t.Errorf("unexpected recycle max size/age: %d/%d", obj.RecycleMaxSize, obj.RecycleMaxAge)
	}
	if obj.TimeMachineMaxSize != "500G" || !obj.TransportEncryption {
		t.Errorf("unexpected time machine fields: maxsize=%q encryption=%v", obj.TimeMachineMaxSize, obj.TransportEncryption)
	}

	var out smbShareResourceModel
	fromSMBShareRPCObject(&obj, &out)

	if out.ID.ValueString() != obj.UUID {
		t.Errorf("ID not round-tripped: got %q want %q", out.ID.ValueString(), obj.UUID)
	}
	if out.RecycleBinMaxSizeBytes.ValueInt64() != 1048576 {
		t.Errorf("recycle_bin_max_size_bytes not round-tripped: %d", out.RecycleBinMaxSizeBytes.ValueInt64())
	}
	if out.RecycleBinRetentionDays.ValueInt64() != 30 {
		t.Errorf("recycle_bin_retention_days not round-tripped: %d", out.RecycleBinRetentionDays.ValueInt64())
	}
	if out.Comment.ValueString() != "media share" {
		t.Errorf("comment not round-tripped: %q", out.Comment.ValueString())
	}
	if out.Guest.ValueString() != "allow" {
		t.Errorf("guest not round-tripped: %q", out.Guest.ValueString())
	}
}

// TestSMBShareCreateReadFlow exercises Create/Read end-to-end against a
// fake Smb service, confirming both the "always send everything"
// request shape and that fields absent from rpc.smb.setshare's schema
// (transportencryption etc.) are still sent and correctly round-tripped.
func TestSMBShareCreateReadFlow(t *testing.T) {
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
		case req.Service == "Smb" && (req.Method == "setShare" || req.Method == "getShare"):
			if req.Method == "setShare" {
				lastSetParams = req.Params
			}
			resp := map[string]interface{}{
				"uuid":                "33333333-3333-3333-3333-333333333333",
				"enable":              true,
				"sharedfolderref":     "11111111-1111-1111-1111-111111111111",
				"comment":             "media share",
				"guest":               "allow",
				"readonly":            false,
				"browseable":          true,
				"recyclebin":          true,
				"recyclemaxsize":      1048576,
				"recyclemaxage":       30,
				"hidedotfiles":        true,
				"inheritacls":         true,
				"inheritpermissions":  false,
				"easupport":           true,
				"storedosattributes":  false,
				"hostsallow":          "192.168.1.0/24",
				"hostsdeny":           "",
				"audit":               false,
				"timemachine":         true,
				"timemachinemaxsize":  "500G",
				"transportencryption": true,
				"followsymlinks":      true,
				"widelinks":           false,
				"extraoptions":        "",
			}
			b, _ := json.Marshal(map[string]interface{}{"response": resp, "error": nil})
			_, _ = w.Write(b)
		case req.Service == "Config" && req.Method == "applyChanges":
			_, _ = w.Write([]byte(`{"response": ["samba"], "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &SMBShareResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := smbShareResourceModel{
		Enabled:                 types.BoolValue(true),
		SharedFolderID:          types.StringValue("11111111-1111-1111-1111-111111111111"),
		Comment:                 types.StringValue("media share"),
		Guest:                   types.StringValue("allow"),
		ReadOnly:                types.BoolValue(false),
		Browseable:              types.BoolValue(true),
		RecycleBin:              types.BoolValue(true),
		RecycleBinMaxSizeBytes:  types.Int64Value(1048576),
		RecycleBinRetentionDays: types.Int64Value(30),
		HideDotFiles:            types.BoolValue(true),
		InheritACLs:             types.BoolValue(true),
		InheritPermissions:      types.BoolValue(false),
		EASupport:               types.BoolValue(true),
		StoreDOSAttributes:      types.BoolValue(false),
		HostsAllow:              types.StringValue("192.168.1.0/24"),
		HostsDeny:               types.StringValue(""),
		Audit:                   types.BoolValue(false),
		TimeMachine:             types.BoolValue(true),
		TimeMachineMaxSize:      types.StringValue("500G"),
		TransportEncryption:     types.BoolValue(true),
		FollowSymlinks:          types.BoolValue(true),
		WideLinks:               types.BoolValue(false),
		ExtraOptions:            types.StringValue(""),
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

	// Confirm fields absent from rpc.smb.setshare's own params schema are
	// still sent in the request (see this file's top doc comment for why
	// that's safe and necessary).
	for _, key := range []string{"timemachinemaxsize", "transportencryption", "followsymlinks", "widelinks"} {
		if _, ok := lastSetParams[key]; !ok {
			t.Errorf("setShare request missing %q (schema-optional but still needs sending to be settable)", key)
		}
	}
	if lastSetParams["transportencryption"] != true {
		t.Errorf("transportencryption not sent correctly: %v", lastSetParams["transportencryption"])
	}

	var created smbShareResourceModel
	createResp.State.Get(context.Background(), &created)
	if created.ID.ValueString() != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("unexpected id after Create: %q", created.ID.ValueString())
	}
	if created.RecycleBinMaxSizeBytes.ValueInt64() != 1048576 {
		t.Errorf("unexpected recycle_bin_max_size_bytes after Create: %d", created.RecycleBinMaxSizeBytes.ValueInt64())
	}
	if !created.TransportEncryption.ValueBool() {
		t.Error("expected transport_encryption to be true after Create")
	}

	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(context.Background(), resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read failed: %v", readResp.Diagnostics)
	}
}
