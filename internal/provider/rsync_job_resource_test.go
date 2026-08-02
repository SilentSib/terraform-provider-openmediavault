package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestRsyncJobToAndFromRPCObject(t *testing.T) {
	ctx := context.Background()
	r := &RsyncJobResource{}

	minute, _ := types.ListValueFrom(ctx, types.StringType, []string{"0", "30"})
	hour, _ := types.ListValueFrom(ctx, types.StringType, []string{"*"})
	month, _ := types.ListValueFrom(ctx, types.StringType, []string{"*"})
	dom, _ := types.ListValueFrom(ctx, types.StringType, []string{"*"})
	dow, _ := types.ListValueFrom(ctx, types.StringType, []string{"1", "3", "5"})

	plan := rsyncJobResourceModel{
		Enable:             types.BoolValue(true),
		SendEmail:          types.BoolValue(false),
		Comment:            types.StringValue("nightly backup"),
		Type:               types.StringValue("remote"),
		Mode:               types.StringValue("push"),
		SrcSharedFolderID:  types.StringValue("11111111-1111-1111-1111-111111111111"),
		SrcURI:             types.StringValue(""),
		DestSharedFolderID: types.StringValue(""),
		DestURI:            types.StringValue("user@example.com:/backups"),
		Minute:             minute,
		EveryNMinute:       types.BoolValue(false),
		Hour:               hour,
		EveryNHour:         types.BoolValue(false),
		Month:              month,
		DayOfMonth:         dom,
		EveryNDayOfMonth:   types.BoolValue(false),
		DayOfWeek:          dow,
		OptionRecursive:    types.BoolValue(true),
		OptionTimes:        types.BoolValue(true),
		OptionGroup:        types.BoolValue(true),
		OptionOwner:        types.BoolValue(true),
		OptionCompress:     types.BoolValue(true),
		OptionArchive:      types.BoolValue(true),
		OptionDelete:       types.BoolValue(false),
		OptionQuiet:        types.BoolValue(true),
		OptionPerms:        types.BoolValue(true),
		OptionACLs:         types.BoolValue(false),
		OptionXattrs:       types.BoolValue(false),
		OptionDryRun:       types.BoolValue(false),
		OptionPartial:      types.BoolValue(false),
		ExtraOptions:       types.StringValue("--bwlimit=1000"),
		Authentication:     types.StringValue("pubkey"),
		Password:           types.StringValue(""),
		SSHCertificateID:   types.StringValue("22222222-2222-2222-2222-222222222222"),
		SSHPort:            types.Int64Value(2222),
	}

	obj, diags := r.toRPCObject(ctx, "33333333-3333-3333-3333-333333333333", &plan)
	if diags.HasError() {
		t.Fatalf("toRPCObject failed: %v", diags)
	}

	if obj.UUID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("unexpected UUID: %s", obj.UUID)
	}
	if obj.Type != "remote" || obj.Mode != "push" {
		t.Errorf("unexpected type/mode: %s/%s", obj.Type, obj.Mode)
	}
	if len(obj.Minute) != 2 || obj.Minute[0] != "0" || obj.Minute[1] != "30" {
		t.Errorf("unexpected minute slice: %v", obj.Minute)
	}
	if len(obj.DayOfWeek) != 3 {
		t.Errorf("unexpected dayofweek slice: %v", obj.DayOfWeek)
	}
	if obj.SSHPort != 2222 {
		t.Errorf("unexpected sshport: %d", obj.SSHPort)
	}
	// Fields not applicable to remote+push should still be present (zero
	// value), matching what OMV's RPC schema requires every key for.
	if obj.SrcURI != "" || obj.DestSharedFolderRef != "" {
		t.Errorf("expected inapplicable fields to be empty, got srcuri=%q destsharedfolderref=%q", obj.SrcURI, obj.DestSharedFolderRef)
	}

	// Now round-trip: simulate what get() would return for this object
	// (same field values; get() always echoes minute/hour/etc as arrays).
	var out rsyncJobResourceModel
	diags = r.fromRPCObject(ctx, &obj, &out)
	if diags.HasError() {
		t.Fatalf("fromRPCObject failed: %v", diags)
	}

	if out.ID.ValueString() != obj.UUID {
		t.Errorf("ID not round-tripped: got %q want %q", out.ID.ValueString(), obj.UUID)
	}
	if out.Comment.ValueString() != "nightly backup" {
		t.Errorf("comment not round-tripped: %q", out.Comment.ValueString())
	}
	if out.SSHPort.ValueInt64() != 2222 {
		t.Errorf("ssh_port not round-tripped: %d", out.SSHPort.ValueInt64())
	}
	var gotMinute []string
	diags = out.Minute.ElementsAs(ctx, &gotMinute, false)
	if diags.HasError() || len(gotMinute) != 2 || gotMinute[0] != "0" || gotMinute[1] != "30" {
		t.Errorf("minute not round-tripped correctly: %v (diags: %v)", gotMinute, diags)
	}

	// fromRPCObject must NOT touch Password (see its doc comment): seed a
	// sentinel value that differs from anything in obj and confirm it
	// survives.
	sentinel := rsyncJobResourceModel{Password: types.StringValue("do-not-touch")}
	sentinel.ID = types.StringValue("keep-me-out-of-the-way")
	var diags2 diag.Diagnostics
	diags2 = r.fromRPCObject(ctx, &obj, &sentinel)
	if diags2.HasError() {
		t.Fatalf("fromRPCObject failed: %v", diags2)
	}
	if sentinel.Password.ValueString() != "do-not-touch" {
		t.Errorf("fromRPCObject must not overwrite Password, got %q", sentinel.Password.ValueString())
	}

	// Simulate the post-import case: Password starts null (ImportState
	// only sets id), and fromRPCObject must give it *some* concrete value
	// rather than leaving it null forever (which would cause a perpetual
	// plan diff -- see the doc comment on fromRPCObject).
	postImport := rsyncJobResourceModel{Password: types.StringNull()}
	diags3 := r.fromRPCObject(ctx, &obj, &postImport)
	if diags3.HasError() {
		t.Fatalf("fromRPCObject failed: %v", diags3)
	}
	if postImport.Password.IsNull() {
		t.Error("fromRPCObject must not leave Password null after a fresh import")
	}
	if postImport.Password.ValueString() != "" {
		t.Errorf("expected the post-import Password fallback to be the schema default \"\", got %q", postImport.Password.ValueString())
	}
}

func TestStringListSliceHelpers(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics

	l, _ := types.ListValueFrom(ctx, types.StringType, []string{"a", "b", "c"})
	got := stringListToSlice(ctx, l, &diags)
	if diags.HasError() {
		t.Fatalf("unexpected diags: %v", diags)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("unexpected slice: %v", got)
	}

	nullList := types.ListNull(types.StringType)
	got = stringListToSlice(ctx, nullList, &diags)
	if diags.HasError() {
		t.Fatalf("unexpected diags for null list: %v", diags)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice for null list, got: %v", got)
	}

	back := sliceToStringList(ctx, []string{"x", "y"}, &diags)
	if diags.HasError() {
		t.Fatalf("unexpected diags: %v", diags)
	}
	var backOut []string
	diags.Append(back.ElementsAs(ctx, &backOut, false)...)
	if len(backOut) != 2 || backOut[0] != "x" || backOut[1] != "y" {
		t.Errorf("unexpected round-tripped slice: %v", backOut)
	}
}
