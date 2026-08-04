package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSSHPublicKeyRegexp(t *testing.T) {
	valid := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBXXXX user@host",
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQD user@host",
		"sk-ssh-ed25519@openssh.com AAAAGnNr user@host",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBXXXX", // comment is optional
		// Reported bug: a key with an underscore-containing comment (a
		// perfectly ordinary OpenSSH comment) followed by a trailing
		// newline -- exactly what Terraform's file() function produces
		// reading a normal .pub file, since nearly all end in "\n". Every
		// case in this block was cross-checked against OMV's actual PHP
		// regex via `php -r` before being pinned here (see
		// sshPublicKeyRegexp's doc comment for why Go's default regex
		// semantics differ from PCRE's here).
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQD id_rsa_omv\n",
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQD id_rsa_omv\r\n", // CRLF line ending
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQD id_rsa_omv",     // same, no trailing newline
	}
	invalid := []string{
		"",
		"not a key at all",
		"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBI user@host",
		"ssh-dss AAAAB3NzaC1kc3MAAA user@host",
		// PHP/PCRE's trailing-newline tolerance is for exactly ONE
		// newline, not more -- confirmed against the real regex, not
		// assumed.
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQD id_rsa_omv\n\n",
	}
	for _, s := range valid {
		if !sshPublicKeyRegexp.MatchString(s) {
			t.Errorf("expected %q to match (valid OpenSSH public key OMV accepts)", s)
		}
	}
	for _, s := range invalid {
		if sshPublicKeyRegexp.MatchString(s) {
			t.Errorf("expected %q NOT to match (rejected by OMV, e.g. ECDSA/DSA, or more than one trailing newline)", s)
		}
	}
}

func TestSSHPrivateKeyRegexp(t *testing.T) {
	valid := []string{
		"-----BEGIN OPENSSH PRIVATE KEY-----\nabc123\n-----END OPENSSH PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----\nabc123\n-----END RSA PRIVATE KEY-----",
	}
	invalid := []string{
		"",
		"garbage",
		"-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----",       // PKCS8, rejected
		"-----BEGIN EC PRIVATE KEY-----\nabc123\n-----END EC PRIVATE KEY-----", // EC, rejected
	}
	for _, s := range valid {
		if !sshPrivateKeyRegexp.MatchString(s) {
			t.Errorf("expected %q to match (format OMV accepts)", s)
		}
	}
	for _, s := range invalid {
		if sshPrivateKeyRegexp.MatchString(s) {
			t.Errorf("expected %q NOT to match (format OMV rejects)", s)
		}
	}
}

func TestSSHCertificateValidateConfig(t *testing.T) {
	r := &SSHCertificateResource{}
	sch := schemaOf(t, r)

	cases := []struct {
		name      string
		config    sshCertificateResourceModel
		expectErr bool
	}{
		{
			name: "empty private key is valid (means: keep existing on update)",
			config: sshCertificateResourceModel{
				PublicKey:     types.StringValue("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBXXXX"),
				PrivateKeyPEM: types.StringValue(""),
				Comment:       types.StringValue(""),
			},
			expectErr: false,
		},
		{
			name: "valid OpenSSH private key",
			config: sshCertificateResourceModel{
				PublicKey:     types.StringValue("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBXXXX"),
				PrivateKeyPEM: types.StringValue("-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"),
				Comment:       types.StringValue(""),
			},
			expectErr: false,
		},
		{
			name: "PKCS8 private key rejected",
			config: sshCertificateResourceModel{
				PublicKey:     types.StringValue("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBXXXX"),
				PrivateKeyPEM: types.StringValue("-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"),
				Comment:       types.StringValue(""),
			},
			expectErr: true,
		},
		{
			name: "unknown private key (not yet planned) is not a false positive",
			config: sshCertificateResourceModel{
				PublicKey:     types.StringValue("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBXXXX"),
				PrivateKeyPEM: types.StringUnknown(),
				Comment:       types.StringValue(""),
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

// TestSSHCertificateGetNeverReturnsPrivateKey mirrors the equivalent SSL
// certificate test: getSsh() strips "privatekey" entirely, so
// fromSSHCertificateRPCObject must preserve whatever's already in state
// and only fall back to "" when state has no value at all (post-import).
func TestSSHCertificateGetNeverReturnsPrivateKey(t *testing.T) {
	raw := `{"uuid": "11111111-1111-1111-1111-111111111111", "publickey": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBXXXX", "comment": "test"}`
	var obj sshCertificateRPCObject
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if obj.PrivateKey != "" {
		t.Fatalf("expected PrivateKey to decode as empty from a realistic getSsh() response, got %q", obj.PrivateKey)
	}

	t.Run("existing non-empty state value is preserved", func(t *testing.T) {
		m := sshCertificateResourceModel{PrivateKeyPEM: types.StringValue("do-not-touch")}
		fromSSHCertificateRPCObject(&obj, &m)
		if m.PrivateKeyPEM.ValueString() != "do-not-touch" {
			t.Errorf("expected PrivateKeyPEM to be preserved, got %q", m.PrivateKeyPEM.ValueString())
		}
	})

	t.Run("null state value (post-import) falls back to the default", func(t *testing.T) {
		m := sshCertificateResourceModel{PrivateKeyPEM: types.StringNull()}
		fromSSHCertificateRPCObject(&obj, &m)
		if m.PrivateKeyPEM.IsNull() {
			t.Fatal("fromSSHCertificateRPCObject must not leave PrivateKeyPEM null after a fresh import")
		}
		if m.PrivateKeyPEM.ValueString() != "" {
			t.Errorf("expected the post-import fallback to be \"\", got %q", m.PrivateKeyPEM.ValueString())
		}
	})
}
