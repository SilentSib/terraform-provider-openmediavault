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

func TestNFSExtraOptionsRegexp(t *testing.T) {
	// Every case here was cross-checked against OMV's actual PHP pattern
	// via `php -r` before being pinned -- including the two non-obvious
	// rejections (empty string, hyphenated values), which an earlier
	// draft of this test got wrong by assuming instead of checking.
	valid := []string{
		"subtree_check",
		"subtree_check,insecure",
		"subtree_check,insecure,async",
		"anonuid=1000",
		"anonuid=1000,anongid=1000",
	}
	invalid := []string{
		"",                       // empty is REJECTED by OMV's own pattern (at least one token required)
		"subtree_check insecure", // space instead of comma
		"subtree_check,",         // trailing comma with nothing after
		",subtree_check",         // leading comma
		"subtree check",          // space within token
		"anonuid=1000 ",          // trailing space
		"fsid=uuid:12345678-1234-1234-1234-123456789012", // hyphens not allowed in values
	}
	for _, s := range valid {
		if !nfsExtraOptionsRegexp.MatchString(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	for _, s := range invalid {
		if nfsExtraOptionsRegexp.MatchString(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

// TestNFSShareMountEntrySentinel pins the trickiest part of this
// resource: toRPCObject must send the omvclient.NewObjectUUID sentinel
// for mount_entry_id when it's null/unknown (the Create case, matching
// exactly what the OMV web UI's own create form does -- see this file's
// top doc comment), but must resend whatever real value is already known
// on Update, since setShare() does NOT re-derive it except on a brand
// new object.
func TestNFSShareMountEntrySentinel(t *testing.T) {
	r := &NFSShareResource{}

	t.Run("create (mount_entry_id unknown): sentinel sent", func(t *testing.T) {
		plan := nfsShareResourceModel{
			SharedFolderID: types.StringValue("sf-uuid"),
			MountEntryID:   types.StringUnknown(),
			Client:         types.StringValue("192.168.1.0/24"),
			Options:        types.StringValue("ro"),
			Comment:        types.StringValue(""),
			ExtraOptions:   types.StringValue(""),
		}
		obj := r.toRPCObject(omvclient.NewObjectUUID, &plan)
		if obj.MountEntryRef != omvclient.NewObjectUUID {
			t.Errorf("expected sentinel %q for unknown mount_entry_id, got %q", omvclient.NewObjectUUID, obj.MountEntryRef)
		}
	})

	t.Run("create (mount_entry_id null): sentinel sent", func(t *testing.T) {
		plan := nfsShareResourceModel{
			SharedFolderID: types.StringValue("sf-uuid"),
			MountEntryID:   types.StringNull(),
			Client:         types.StringValue("192.168.1.0/24"),
			Options:        types.StringValue("ro"),
			Comment:        types.StringValue(""),
			ExtraOptions:   types.StringValue(""),
		}
		obj := r.toRPCObject(omvclient.NewObjectUUID, &plan)
		if obj.MountEntryRef != omvclient.NewObjectUUID {
			t.Errorf("expected sentinel %q for null mount_entry_id, got %q", omvclient.NewObjectUUID, obj.MountEntryRef)
		}
	})

	t.Run("update (mount_entry_id known): real value resent verbatim", func(t *testing.T) {
		plan := nfsShareResourceModel{
			SharedFolderID: types.StringValue("sf-uuid"),
			MountEntryID:   types.StringValue("real-mount-entry-uuid"),
			Client:         types.StringValue("192.168.1.0/24"),
			Options:        types.StringValue("rw"),
			Comment:        types.StringValue(""),
			ExtraOptions:   types.StringValue(""),
		}
		obj := r.toRPCObject("existing-share-uuid", &plan)
		if obj.MountEntryRef != "real-mount-entry-uuid" {
			t.Errorf("expected the real mount_entry_id to be resent verbatim, got %q", obj.MountEntryRef)
		}
		if obj.UUID != "existing-share-uuid" {
			t.Errorf("expected the share's own UUID to be passed through, got %q", obj.UUID)
		}
	})
}

// TestNFSShareCreateUpdateFlow exercises Create then Update end-to-end
// against a fake Nfs service, confirming: Create sends the sentinel and
// stores whatever mntentref OMV actually assigned; Update resends that
// real value rather than the sentinel again.
func TestNFSShareCreateUpdateFlow(t *testing.T) {
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
		case req.Service == "Nfs" && req.Method == "setShare":
			lastSetParams = req.Params
			// Simulate OMV: on create (mntentref == sentinel), assign a
			// real bind-mount UUID; on update, echo back whatever was sent.
			mntentref := req.Params["mntentref"]
			if mntentref == omvclient.NewObjectUUID {
				mntentref = "real-mount-entry-uuid"
			}
			resp := map[string]interface{}{
				"uuid":            "share-uuid-1",
				"sharedfolderref": req.Params["sharedfolderref"],
				"mntentref":       mntentref,
				"client":          req.Params["client"],
				"options":         req.Params["options"],
				"comment":         req.Params["comment"],
				"extraoptions":    req.Params["extraoptions"],
			}
			b, _ := json.Marshal(map[string]interface{}{"response": resp, "error": nil})
			_, _ = w.Write(b)
		case req.Service == "Config" && req.Method == "applyChanges":
			_, _ = w.Write([]byte(`{"response": ["nfs", "fstab"], "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &NFSShareResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := nfsShareResourceModel{
		SharedFolderID: types.StringValue("sf-uuid"),
		MountEntryID:   types.StringUnknown(),
		Client:         types.StringValue("192.168.1.0/24"),
		Options:        types.StringValue("ro"),
		Comment:        types.StringValue(""),
		ExtraOptions:   types.StringValue(""),
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
	if lastSetParams["mntentref"] != omvclient.NewObjectUUID {
		t.Fatalf("expected Create to send the sentinel mntentref, got %v", lastSetParams["mntentref"])
	}

	var created nfsShareResourceModel
	createResp.State.Get(context.Background(), &created)
	if created.MountEntryID.ValueString() != "real-mount-entry-uuid" {
		t.Fatalf("expected the real assigned mount_entry_id to be stored, got %q", created.MountEntryID.ValueString())
	}

	// Now Update: change "options" to "rw", keeping everything else from
	// the Create response (as Terraform would, since mount_entry_id is
	// Computed and UseStateForUnknown carries the prior value forward).
	updatePlan := created
	updatePlan.Options = types.StringValue("rw")
	updatePlanTF := tfsdk.Plan{Schema: sch.Schema}
	if diags := updatePlanTF.Set(context.Background(), &updatePlan); diags.HasError() {
		t.Fatalf("failed to build update plan: %v", diags)
	}

	updateResp := resource.UpdateResponse{State: createResp.State}
	r.Update(context.Background(), resource.UpdateRequest{Plan: updatePlanTF, State: createResp.State}, &updateResp)
	if updateResp.Diagnostics.HasError() {
		t.Fatalf("Update failed: %v", updateResp.Diagnostics)
	}

	if lastSetParams["mntentref"] != "real-mount-entry-uuid" {
		t.Errorf("expected Update to resend the REAL mount_entry_id, not the sentinel: got %v", lastSetParams["mntentref"])
	}
	if lastSetParams["options"] != "rw" {
		t.Errorf("expected options to have changed to \"rw\": got %v", lastSetParams["options"])
	}
}
