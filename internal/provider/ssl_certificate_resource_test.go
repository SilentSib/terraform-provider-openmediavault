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

func TestCertificatePEMPrefixRegexp(t *testing.T) {
	valid := []string{
		"-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----\n",
		"  -----BEGIN CERTIFICATE-----\nMIIB...", // leading whitespace tolerated
	}
	invalid := []string{
		"",
		"not a certificate",
		"-----BEGIN PRIVATE KEY-----\n...",
		"-----BEGIN RSA PRIVATE KEY-----\n...",
	}
	for _, s := range valid {
		if !certificatePEMPrefixRegexp.MatchString(s) {
			t.Errorf("expected %q to match the certificate PEM prefix", s)
		}
	}
	for _, s := range invalid {
		if certificatePEMPrefixRegexp.MatchString(s) {
			t.Errorf("expected %q NOT to match the certificate PEM prefix", s)
		}
	}
}

// TestSSLCertificateGetNeverReturnsPrivateKey pins the exact behavior
// found in source: CertificateMgmt.get() strips "privatekey" from its
// response entirely. fromSSLCertificateRPCObject must never populate
// PrivateKeyPEM from such a response (except the null/unknown post-import
// fallback case).
func TestSSLCertificateGetNeverReturnsPrivateKey(t *testing.T) {
	// Simulates decoding a real get() response, which simply omits
	// "privatekey" -- PrivateKey stays "" (Go zero value) after decode.
	raw := `{"uuid": "11111111-1111-1111-1111-111111111111", "certificate": "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n", "comment": "test"}`
	var obj sslCertificateRPCObject
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if obj.PrivateKey != "" {
		t.Fatalf("expected PrivateKey to decode as empty from a realistic get() response, got %q", obj.PrivateKey)
	}

	t.Run("existing non-empty state value is preserved", func(t *testing.T) {
		m := sslCertificateResourceModel{PrivateKeyPEM: types.StringValue("do-not-touch")}
		fromSSLCertificateRPCObject(&obj, &m)
		if m.PrivateKeyPEM.ValueString() != "do-not-touch" {
			t.Errorf("expected PrivateKeyPEM to be preserved, got %q", m.PrivateKeyPEM.ValueString())
		}
		if m.CertificatePEM.IsNull() || m.Comment.ValueString() != "test" {
			t.Errorf("expected certificate/comment to be refreshed from the response: cert=%q comment=%q",
				m.CertificatePEM.ValueString(), m.Comment.ValueString())
		}
	})

	t.Run("null state value (post-import) falls back to the default", func(t *testing.T) {
		m := sslCertificateResourceModel{PrivateKeyPEM: types.StringNull()}
		fromSSLCertificateRPCObject(&obj, &m)
		if m.PrivateKeyPEM.IsNull() {
			t.Fatal("fromSSLCertificateRPCObject must not leave PrivateKeyPEM null after a fresh import")
		}
		if m.PrivateKeyPEM.ValueString() != "" {
			t.Errorf("expected the post-import fallback to be \"\", got %q", m.PrivateKeyPEM.ValueString())
		}
	})
}

// TestSSLCertificateCreateReadFlow exercises Create/Read end-to-end
// against a fake CertificateMgmt service.
func TestSSLCertificateCreateReadFlow(t *testing.T) {
	const certPEM = "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"
	const keyPEM = "-----BEGIN PRIVATE KEY-----\ndef\n-----END PRIVATE KEY-----\n"

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
		case req.Service == "CertificateMgmt" && req.Method == "set":
			lastSetParams = req.Params
			// set()'s response DOES include privatekey (unlike get()).
			resp := map[string]interface{}{
				"uuid":        "22222222-2222-2222-2222-222222222222",
				"certificate": certPEM,
				"privatekey":  keyPEM,
				"comment":     "my cert",
			}
			b, _ := json.Marshal(map[string]interface{}{"response": resp, "error": nil})
			_, _ = w.Write(b)
		case req.Service == "CertificateMgmt" && req.Method == "get":
			// get()'s response never includes privatekey.
			resp := map[string]interface{}{
				"uuid":        "22222222-2222-2222-2222-222222222222",
				"certificate": certPEM,
				"comment":     "my cert",
			}
			b, _ := json.Marshal(map[string]interface{}{"response": resp, "error": nil})
			_, _ = w.Write(b)
		case req.Service == "Config" && req.Method == "applyChanges":
			_, _ = w.Write([]byte(`{"response": ["certificates"], "error": null}`))
		default:
			t.Fatalf("unexpected call: %s.%s", req.Service, req.Method)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &SSLCertificateResource{client: mustClientForHostPort(t, srv)}
	sch := schemaOf(t, r)

	plan := sslCertificateResourceModel{
		CertificatePEM: types.StringValue(certPEM),
		PrivateKeyPEM:  types.StringValue(keyPEM),
		Comment:        types.StringValue("my cert"),
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

	if lastSetParams["certificate"] != certPEM || lastSetParams["privatekey"] != keyPEM {
		t.Error("set() request did not carry through the configured certificate/private key")
	}
	if _, ok := lastSetParams["comment"]; !ok {
		t.Error("set() request missing required \"comment\" field")
	}

	var created sslCertificateResourceModel
	createResp.State.Get(context.Background(), &created)
	if created.ID.ValueString() != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("unexpected id after Create: %q", created.ID.ValueString())
	}
	if created.PrivateKeyPEM.ValueString() != keyPEM {
		t.Errorf("expected Create to keep the plan's private key value, got %q", created.PrivateKeyPEM.ValueString())
	}

	// Read: get()'s response has no privatekey. Confirm the resource
	// preserves the already-known state value instead of blanking it.
	readResp := resource.ReadResponse{State: createResp.State}
	r.Read(context.Background(), resource.ReadRequest{State: createResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("Read failed: %v", readResp.Diagnostics)
	}
	var afterRead sslCertificateResourceModel
	readResp.State.Get(context.Background(), &afterRead)
	if afterRead.PrivateKeyPEM.ValueString() != keyPEM {
		t.Errorf("Read must not blank private_key_pem (get() never returns it): got %q, want %q",
			afterRead.PrivateKeyPEM.ValueString(), keyPEM)
	}
	if afterRead.CertificatePEM.ValueString() != certPEM {
		t.Errorf("Read should refresh certificate_pem from the response: got %q", afterRead.CertificatePEM.ValueString())
	}
}
