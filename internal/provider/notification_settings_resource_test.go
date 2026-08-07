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

func TestNotificationEmailRegexp(t *testing.T) {
	valid := []string{"", "admin@example.com", "a@b.co", "first.last+tag@sub.example.com"}
	invalid := []string{"not-an-email", "@example.com", "admin@", "admin @example.com", "admin@example"}
	for _, s := range valid {
		if !notificationEmailRegexp.MatchString(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	for _, s := range invalid {
		if notificationEmailRegexp.MatchString(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestNotificationSettingsValidateConfig(t *testing.T) {
	r := &NotificationSettingsResource{}
	sch := schemaOf(t, r)

	cases := []struct {
		name      string
		config    notificationSettingsResourceModel
		expectErr bool
	}{
		{
			name: "disabled: no requirements",
			config: notificationSettingsResourceModel{
				Enabled: types.BoolValue(false), AuthEnabled: types.BoolValue(false),
				SMTPServer: types.StringValue(""), SenderEmail: types.StringValue(""),
				PrimaryEmail: types.StringValue(""), SMTPUsername: types.StringValue(""),
				SMTPPassword: types.StringValue(""), SecondaryEmail: types.StringValue(""),
			},
			expectErr: false,
		},
		{
			name: "enabled with everything set: fine",
			config: notificationSettingsResourceModel{
				Enabled: types.BoolValue(true), AuthEnabled: types.BoolValue(false),
				SMTPServer: types.StringValue("smtp.example.com"), SenderEmail: types.StringValue("a@b.com"),
				PrimaryEmail: types.StringValue("c@d.com"), SMTPUsername: types.StringValue(""),
				SMTPPassword: types.StringValue(""), SecondaryEmail: types.StringValue(""),
			},
			expectErr: false,
		},
		{
			name: "enabled but missing server: error",
			config: notificationSettingsResourceModel{
				Enabled: types.BoolValue(true), AuthEnabled: types.BoolValue(false),
				SMTPServer: types.StringValue(""), SenderEmail: types.StringValue("a@b.com"),
				PrimaryEmail: types.StringValue("c@d.com"), SMTPUsername: types.StringValue(""),
				SMTPPassword: types.StringValue(""), SecondaryEmail: types.StringValue(""),
			},
			expectErr: true,
		},
		{
			name: "auth enabled but missing username: error",
			config: notificationSettingsResourceModel{
				Enabled: types.BoolValue(false), AuthEnabled: types.BoolValue(true),
				SMTPServer: types.StringValue(""), SenderEmail: types.StringValue(""),
				PrimaryEmail: types.StringValue(""), SMTPUsername: types.StringValue(""),
				SMTPPassword: types.StringValue("secret"), SecondaryEmail: types.StringValue(""),
			},
			expectErr: true,
		},
		{
			name: "enabled but server unknown (not yet defaulted): no false positive",
			config: notificationSettingsResourceModel{
				Enabled: types.BoolValue(true), AuthEnabled: types.BoolValue(false),
				SMTPServer: types.StringUnknown(), SenderEmail: types.StringValue("a@b.com"),
				PrimaryEmail: types.StringValue("c@d.com"), SMTPUsername: types.StringValue(""),
				SMTPPassword: types.StringValue(""), SecondaryEmail: types.StringValue(""),
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

func TestNotificationSettingsDeleteDoesNotCallRPC(t *testing.T) {
	r := &NotificationSettingsResource{}
	sch := schemaOf(t, r)

	state := notificationSettingsResourceModel{ID: types.StringValue(notificationSettingsID)}
	stateTF := tfsdk.State{Schema: sch.Schema}
	if diags := stateTF.Set(context.Background(), &state); diags.HasError() {
		t.Fatalf("failed to build state: %v", diags)
	}

	resp := resource.DeleteResponse{State: stateTF}
	r.Delete(context.Background(), resource.DeleteRequest{State: stateTF}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete returned unexpected error diagnostics: %v", resp.Diagnostics)
	}
	found := false
	for _, d := range resp.Diagnostics {
		if d.Summary() == "Notification Settings Left Unchanged" {
			found = true
		}
	}
	if !found {
		t.Error("expected the 'Notification Settings Left Unchanged' warning diagnostic")
	}
}

func TestNotificationSettingsImportRejectsWrongID(t *testing.T) {
	r := &NotificationSettingsResource{}
	sch := schemaOf(t, r)

	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sch.Schema}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "wrong"}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error importing with an ID other than the fixed synthetic one")
	}
}

// TestNotificationSettingsCreateHandlesSetGetDivergence is the most
// important test in this file: it pins the exact bug this resource's
// setThenRefetch works around. set()'s response has authenable/username/
// password nested under "authentication" with no flat aliases; get()'s
// response has them flat. If Create ever regressed to decoding set()'s
// response directly (the same mistake originally made for
// omv_rsync_job), authenticated fields would silently come back zeroed
// out in state even though the correct values were actually stored --
// this test would catch that by asserting the POST-Create state reflects
// what get() returned, not zero values.
func TestNotificationSettingsCreateHandlesSetGetDivergence(t *testing.T) {
	var getCallCount int
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
		case req.Service == "EmailNotification" && req.Method == "set":
			// Realistic set() response: raw object, authentication NESTED,
			// no flat authenable/username/password aliases at all.
			_, _ = w.Write([]byte(`{"response": {
				"enable": true,
				"server": "smtp.example.com",
				"port": 587,
				"tls": "starttls",
				"sender": "nas@example.com",
				"authentication": {"enable": true, "username": "smtpuser", "password": "smtppass"},
				"primaryemail": "admin@example.com",
				"secondaryemail": ""
			}, "error": null}`))
		case req.Service == "EmailNotification" && req.Method == "get":
			getCallCount++
			// Realistic get() response: flat, matching what this resource
			// actually decodes.
			_, _ = w.Write([]byte(`{"response": {
				"enable": true,
				"server": "smtp.example.com",
				"port": 587,
				"tls": "starttls",
				"sender": "nas@example.com",
				"authenable": true,
				"username": "smtpuser",
				"password": "smtppass",
				"primaryemail": "admin@example.com",
				"secondaryemail": ""
			}, "error": null}`))
		case req.Service == "Config" && req.Method == "applyChanges":
			_, _ = w.Write([]byte(`{"response": ["postfix"], "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &NotificationSettingsResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := notificationSettingsResourceModel{
		Enabled: types.BoolValue(true), SMTPServer: types.StringValue("smtp.example.com"),
		SMTPPort: types.Int64Value(587), EncryptionMode: types.StringValue("starttls"),
		SenderEmail: types.StringValue("nas@example.com"), AuthEnabled: types.BoolValue(true),
		SMTPUsername: types.StringValue("smtpuser"), SMTPPassword: types.StringValue("smtppass"),
		PrimaryEmail: types.StringValue("admin@example.com"), SecondaryEmail: types.StringValue(""),
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

	if getCallCount != 1 {
		t.Fatalf("expected exactly one get() refetch call after set(), got %d", getCallCount)
	}

	var created notificationSettingsResourceModel
	createResp.State.Get(context.Background(), &created)

	// The critical assertions: these must reflect get()'s flat response,
	// NOT be zeroed out as they would be if set()'s nested response had
	// been decoded directly.
	if !created.AuthEnabled.ValueBool() {
		t.Error("auth_enabled was zeroed out -- set()/get() divergence bug regressed")
	}
	if created.SMTPUsername.ValueString() != "smtpuser" {
		t.Errorf("smtp_username was zeroed out (got %q) -- set()/get() divergence bug regressed", created.SMTPUsername.ValueString())
	}
	if created.SMTPPassword.ValueString() != "smtppass" {
		t.Errorf("smtp_password was zeroed out (got %q) -- set()/get() divergence bug regressed", created.SMTPPassword.ValueString())
	}
	if created.ID.ValueString() != notificationSettingsID {
		t.Errorf("unexpected id: %q", created.ID.ValueString())
	}
}
