package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	fwstringvalidator "github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// This resource manages the SSH tab of OMV's "System > Certificates" page
// -- the sibling of omv_ssl_certificate, same "CertificateMgmt" RPC
// service, separate "*Ssh" methods and config object
// ("conf.system.certificate.ssh", referenced elsewhere as
// "sshcertificateref" -- e.g. omv_rsync_job's ssh_certificate_id).
// Verified against the OMV 8.5.5 source (engined/rpc/certificatemgmt.inc,
// datamodel/schema.inc's "sshpubkey-openssh"/"sshprivkey-pem" format
// validators, and the rpc.certificatemgmt/conf.system.certificate.ssh
// datamodels):
//   - methods "getSsh"/"setSsh"/"deleteSsh" for basic CRUD (there's also
//     "getSshList", "copySshId", and "createSsh" -- the latter runs
//     ssh-keygen server-side. As with omv_ssl_certificate/
//     CertificateMgmt.create(), this resource intentionally does NOT
//     wrap key generation: use the community hashicorp/tls provider's
//     tls_private_key (which can emit an OpenSSH-formatted private key
//     via its private_key_openssh attribute) instead.
//   - getSsh() strips "privatekey" from its response, same as get() does
//     for SSL certs -- same never-refreshed-from-Read,
//     falls-back-to-empty-after-import treatment as
//     omv_ssl_certificate.private_key_pem / omv_rsync_job.password.
//   - "publickey" and "comment" are required by rpc.certificatemgmt.
//     json's "setSsh" schema; "privatekey" is not (empty reuses the
//     already-stored key on an update, only erroring if empty on a
//     brand-new object).
//   - IMPORTANT FORMAT CONSTRAINT, easy to trip over: OMV's own format
//     validators are narrower than "any valid SSH key". "publickey" must
//     match \OMV\Ssh\PublicKey::isOpenSSH(), which only accepts
//     "ssh-rsa", "ssh-ed25519", or "sk-ssh-ed25519@openssh.com" --
//     notably NOT ecdsa-sha2-* or ssh-dss keys. "privatekey" must be PEM
//     starting with "-----BEGIN OPENSSH PRIVATE KEY-----" or
//     "-----BEGIN RSA PRIVATE KEY-----" -- notably NOT the generic PKCS8
//     "-----BEGIN PRIVATE KEY-----" that e.g. tls_private_key's
//     private_key_pem attribute produces for ed25519/ECDSA keys (only its
//     RSA output happens to use the accepted "RSA PRIVATE KEY" form), and
//     NOT "-----BEGIN EC PRIVATE KEY-----" either. In practice: use RSA
//     with private_key_pem, or any supported algorithm with
//     private_key_openssh. This resource validates both formats
//     client-side (mirroring OMV's exact regexes) so a mismatch is caught
//     at `terraform plan` rather than surfacing as an opaque RPC error.
//   - deleting requires the certificate not be referenced elsewhere
//     (db->assertIsNotReferenced()) -- e.g. by omv_rsync_job's
//     ssh_certificate_id.
//   - creating/modifying/deleting a SSH certificate marks the same
//     "certificates" engine module dirty as SSL certificates do
//     (Certificates::bindListeners() covers both config paths).
var (
	_ resource.Resource                   = &SSHCertificateResource{}
	_ resource.ResourceWithConfigure      = &SSHCertificateResource{}
	_ resource.ResourceWithImportState    = &SSHCertificateResource{}
	_ resource.ResourceWithValidateConfig = &SSHCertificateResource{}
)

// dirtiedBySSHCertificateChanges is the engine module OMV marks dirty
// whenever a SSH (or SSL) certificate is created, modified, or deleted
// (Certificates::bindListeners() in engined/module/certificates.inc).
var dirtiedBySSHCertificateChanges = []string{"certificates"}

// sshPublicKeyRegexp mirrors \OMV\Ssh\PublicKey::isOpenSSH() verbatim
// (usr/share/php/openmediavault/ssh/publickey.inc): only "ssh-rsa",
// "ssh-ed25519", or "sk-ssh-ed25519@openssh.com" are accepted -- notably
// NOT ecdsa-sha2-* or ssh-dss.
//
// The trailing `\r?\n?` is NOT in OMV's own pattern -- it compensates for
// a real difference between PHP/PCRE and Go/RE2, found via a reported
// false-rejection of a valid key ending in a comment (e.g. "id_rsa_omv")
// with a trailing newline, which is exactly what Terraform's file()
// function produces reading an ordinary .pub file (nearly all end in
// "\n"). PCRE's `$` (used by OMV's actual regex, unlike Go's RE2 default)
// matches at the end of the subject OR immediately before a single
// trailing newline -- Go's default `$` only matches at the absolute end
// of text, with no such allowance. Verified empirically (not just from
// memory of the spec) by running OMV's literal PHP regex via `php -r`
// against a matrix of trailing-newline/CRLF/whitespace cases, then
// confirming this Go pattern produces identical results for every case,
// including the boundary one PCRE itself rejects: exactly one trailing
// "\n" is accepted, two are not.
var sshPublicKeyRegexp = regexp.MustCompile(
	`^(sk-ssh-ed25519@openssh\.com|ssh-(rsa|ed25519)) AAAA[0-9A-Za-z+/]+={0,3}\s*(.+)*\r?\n?$`,
)

// sshPrivateKeyRegexp mirrors the "sshprivkey-pem" format validator in
// datamodel/schema.inc verbatim: only PEM blocks headed "OPENSSH PRIVATE
// KEY" or "RSA PRIVATE KEY" are accepted -- notably NOT the generic PKCS8
// "PRIVATE KEY" header or "EC PRIVATE KEY".
var sshPrivateKeyRegexp = regexp.MustCompile(
	`(?sm)^-----BEGIN (OPENSSH|RSA) PRIVATE KEY-----[\r\n\f](.+)[\r\n\f]-----END (OPENSSH|RSA) PRIVATE KEY-----$`,
)

func NewSSHCertificateResource() resource.Resource {
	return &SSHCertificateResource{}
}

// SSHCertificateResource implements the omv_ssh_certificate resource.
type SSHCertificateResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// sshCertificateResourceModel maps omv_ssh_certificate schema <-> Go.
type sshCertificateResourceModel struct {
	ID            types.String `tfsdk:"id"`
	PublicKey     types.String `tfsdk:"public_key_openssh"`
	PrivateKeyPEM types.String `tfsdk:"private_key_pem"`
	Comment       types.String `tfsdk:"comment"`
}

func (r *SSHCertificateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ssh_certificate"
}

func (r *SSHCertificateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a SSH key pair under OpenMediaVault's System > Certificates page (the SSH " +
			"tab; SSL certificates are the sibling omv_ssl_certificate resource). A \"bring your own key\" " +
			"resource -- generate key material with the community hashicorp/tls provider (or any other " +
			"source) and pass it in here. IMPORTANT: OMV only accepts a narrower set of formats than " +
			"\"any valid SSH key\" -- see this resource's attribute descriptions and this file's top doc " +
			"comment for the exact constraints (notably: no ECDSA public keys, no PKCS8/EC private keys).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID assigned by OMV to this SSH certificate.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"public_key_openssh": schema.StringAttribute{
				Required: true,
				Description: "OpenSSH-format public key (\"ssh-<type> <base64> [comment]\"). Only " +
					"ssh-rsa, ssh-ed25519, and sk-ssh-ed25519@openssh.com are accepted by OMV -- " +
					"ECDSA (ecdsa-sha2-*) and DSA (ssh-dss) keys are rejected. A single trailing " +
					"newline (as produced by file() reading an ordinary .pub file) is fine.",
				Validators: []validator.String{
					fwstringvalidator.RegexMatches(
						sshPublicKeyRegexp,
						"must be an OpenSSH-format public key using ssh-rsa, ssh-ed25519, or sk-ssh-ed25519@openssh.com (ECDSA and DSA keys are not accepted by OMV)",
					),
				},
			},
			"private_key_pem": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Sensitive: true,
				Description: "PEM-encoded private key corresponding to public_key_openssh. Must start " +
					"with \"-----BEGIN OPENSSH PRIVATE KEY-----\" or \"-----BEGIN RSA PRIVATE KEY-----\" -- " +
					"OMV does NOT accept the generic PKCS8 \"-----BEGIN PRIVATE KEY-----\" or " +
					"\"-----BEGIN EC PRIVATE KEY-----\" forms (checked client-side; skipped when this is " +
					"\"\", see below). Required by OMV when creating a new key pair (not enforced client-" +
					"side; OMV's own \"Private key does not exist.\" error surfaces if omitted on create). " +
					"On an update, explicitly setting this to \"\" tells OMV to keep the already-stored " +
					"private key rather than replacing it. Never returned by OMV's read API (deliberately " +
					"stripped server-side), so this provider can't verify or refresh it from the server.",
			},
			"comment": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Free-form description shown in the OMV UI.",
			},
		},
	}
}

func (r *SSHCertificateResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config sshCertificateResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// private_key_pem's format is only checked when non-empty: "" is a
	// meaningful, valid value (see its schema description -- it tells OMV
	// to keep the already-stored key on an update), and a plain schema
	// validator can't express "validate this format, but only if it's not
	// empty" the way stringvalidator.RegexMatches works.
	if !config.PrivateKeyPEM.IsNull() && !config.PrivateKeyPEM.IsUnknown() {
		key := config.PrivateKeyPEM.ValueString()
		if key != "" && !sshPrivateKeyRegexp.MatchString(key) {
			resp.Diagnostics.AddAttributeError(
				path.Root("private_key_pem"),
				"Invalid SSH Private Key Format",
				"must be PEM starting with \"-----BEGIN OPENSSH PRIVATE KEY-----\" or \"-----BEGIN RSA "+
					"PRIVATE KEY-----\" (OMV does not accept PKCS8 \"-----BEGIN PRIVATE KEY-----\" or "+
					"\"-----BEGIN EC PRIVATE KEY-----\"), or \"\" to keep an existing stored key on update.",
			)
		}
	}
}

func (r *SSHCertificateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a provider bug, please report it.", req.ProviderData),
		)
		return
	}
	r.client = pd.Client
	r.revertOnApplyFailure = pd.RevertOnApplyFailure
}

// sshCertificateRPCObject is the shape of a SSH certificate object as
// consumed/returned by CertificateMgmt's getSsh/setSsh methods. Both are
// safe to decode with this same struct, EXCEPT that getSsh() never
// populates PrivateKey -- see this file's top doc comment.
type sshCertificateRPCObject struct {
	UUID       string `json:"uuid"`
	PublicKey  string `json:"publickey"`
	PrivateKey string `json:"privatekey,omitempty"`
	Comment    string `json:"comment"`
}

func (r *SSHCertificateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sshCertificateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := sshCertificateRPCObject{
		UUID:       omvclient.NewObjectUUID,
		PublicKey:  plan.PublicKey.ValueString(),
		PrivateKey: plan.PrivateKeyPEM.ValueString(),
		Comment:    plan.Comment.ValueString(),
	}

	var created sshCertificateRPCObject
	if err := r.client.Call(ctx, "CertificateMgmt", "setSsh", obj, &created); err != nil {
		resp.Diagnostics.AddError("Error Creating SSH Certificate", err.Error())
		return
	}

	fromSSHCertificateRPCObject(&created, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SSHCertificateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sshCertificateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found sshCertificateRPCObject
	err := r.client.Call(ctx, "CertificateMgmt", "getSsh", map[string]string{
		"uuid": state.ID.ValueString(),
	}, &found)
	if err != nil {
		// TODO: as elsewhere in this provider, match the specific "object
		// does not exist" RPC error and call resp.State.RemoveResource(ctx)
		// instead, so certificates deleted out of band can be recreated.
		resp.Diagnostics.AddError("Error Reading SSH Certificate", err.Error())
		return
	}

	fromSSHCertificateRPCObject(&found, &state)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *SSHCertificateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sshCertificateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := sshCertificateRPCObject{
		UUID:       plan.ID.ValueString(),
		PublicKey:  plan.PublicKey.ValueString(),
		PrivateKey: plan.PrivateKeyPEM.ValueString(),
		Comment:    plan.Comment.ValueString(),
	}

	var updated sshCertificateRPCObject
	if err := r.client.Call(ctx, "CertificateMgmt", "setSsh", obj, &updated); err != nil {
		resp.Diagnostics.AddError("Error Updating SSH Certificate", err.Error())
		return
	}

	fromSSHCertificateRPCObject(&updated, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SSHCertificateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sshCertificateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Call(ctx, "CertificateMgmt", "deleteSsh", map[string]string{
		"uuid": state.ID.ValueString(),
	}, nil)
	if err != nil {
		// deleteSsh() calls assertIsNotReferenced() before mutating
		// anything -- if this key is still referenced (e.g. by
		// omv_rsync_job.ssh_certificate_id), OMV rejects the delete with
		// an RPC error, which surfaces here verbatim.
		resp.Diagnostics.AddError("Error Deleting SSH Certificate", err.Error())
		return
	}

	if _, err := r.client.ApplyChanges(ctx, dirtiedBySSHCertificateChanges, false); err != nil {
		resp.Diagnostics.AddWarning(
			"SSH Certificate Deleted, but Deploying the Change Failed",
			fmt.Sprintf(
				"The SSH certificate was removed from OMV's configuration, but deploying that removal "+
					"(Config.applyChanges) failed: %s. Its key files under OMV's SSH keys directory may "+
					"still be present on disk until this is resolved (retry from the OMV web UI's pending "+
					"changes panel).",
				err,
			),
		)
	}
}

func (r *SSHCertificateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// fromSSHCertificateRPCObject copies an RPC response back into the
// Terraform model. Mirrors fromSSLCertificateRPCObject's treatment of
// PrivateKeyPEM exactly -- see that function's doc comment in
// ssl_certificate_resource.go for the full rationale (getSsh() strips it
// server-side; left untouched except for the null/unknown post-import
// case, which falls back to "").
func fromSSHCertificateRPCObject(obj *sshCertificateRPCObject, m *sshCertificateResourceModel) {
	m.ID = types.StringValue(obj.UUID)
	m.PublicKey = types.StringValue(obj.PublicKey)
	m.Comment = types.StringValue(obj.Comment)
	if m.PrivateKeyPEM.IsNull() || m.PrivateKeyPEM.IsUnknown() {
		m.PrivateKeyPEM = types.StringValue("")
	}
}

// applyOrHandleApplyFailure delegates to the shared implementation in
// apply_helper.go (see its doc comment for the full rationale, including
// the client-side-timeout-vs-real-failure distinction), scoped to the
// modules a SSHCertificateResource change dirties.
func (r *SSHCertificateResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	applyOrHandleApplyFailure(ctx, r.client, r.revertOnApplyFailure, dirtiedBySSHCertificateChanges, diags)
}
