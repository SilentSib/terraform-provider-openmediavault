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

// This resource manages OMV's "System > Certificates" page's SSL tab
// (the SSH tab is a separate config object/RPC methods, "conf.system.
// certificate.ssh" / CertificateMgmt.{get,set,delete}Ssh -- not covered
// by this resource; it's a natural next addition following the same
// pattern). Verified against the OMV 8.5.5 source
// (engined/rpc/certificatemgmt.inc, engined/module/certificates.inc, and
// the rpc.certificatemgmt/conf.system.certificate.ssl datamodels):
//   - service "CertificateMgmt", methods "get"/"set"/"delete" for basic
//     CRUD (there's also "getList", "getDetail", and "create" -- the
//     latter has OMV generate a new self-signed cert+key server-side from
//     subject fields (size/days/c/st/l/o/ou/cn/email) and then just calls
//     "set" with the result. This resource intentionally does NOT wrap
//     "create": generating key material is exactly what the community
//     `hashicorp/tls` provider's `tls_private_key`/`tls_self_signed_cert`
//     resources are for, and feeding their output into this resource's
//     certificate_pem/private_key_pem is the idiomatic Terraform pattern
//     (matches how most other providers' "bring your own certificate"
//     resources work), rather than this provider growing its own
//     redundant, less-flexible cert generation path.
//   - CertificateMgmt.get() explicitly strips "privatekey" from its
//     response ("Remove the private key. It should not leave the
//     system.") -- get() is used for both the getList() datatable AND
//     this resource's Read(), so private_key_pem can never be refreshed
//     from a read the way certificate_pem and comment can. Same treatment
//     as omv_rsync_job's password: left untouched on a normal Read, with
//     the same null/unknown (post-import) fallback used there -- see
//     fromRPCObject's doc comment below and rsync_job_resource.go's
//     equivalent.
//   - "privatekey" is NOT required by rpc.certificatemgmt.json's "set"
//     schema (unlike "certificate" and "comment", which are): if omitted
//     (empty) on an update, set() reuses whatever private key is already
//     stored, and only errors if empty on a brand new object. This
//     provider always sends whatever's in the plan (see toRPCObject), so
//     that reuse path is really only reachable by explicitly setting
//     private_key_pem = "" in configuration on an update -- Terraform's
//     model doesn't have an "unspecified means leave alone" concept the
//     way the raw RPC does.
//   - deleting requires the certificate not be referenced elsewhere
//     (db->assertIsNotReferenced()) -- e.g. by omv_workbench_settings's
//     ssl_certificate_id. OMV surfaces that as an RPC error, which this
//     resource's Delete() passes straight through.
//   - creating/modifying/deleting a certificate marks the "certificates"
//     engine module dirty (engined/module/certificates.inc), which writes
//     the actual .crt/.key files to OMV_SSL_CERTIFICATE_DIR on disk via
//     Config.applyChanges -- functionally required (like omv_rsync_job's
//     "rsync" module), not just best-effort tidiness, since services that
//     reference the certificate read it from those files, not the config
//     database.
var (
	_ resource.Resource                = &SSLCertificateResource{}
	_ resource.ResourceWithConfigure   = &SSLCertificateResource{}
	_ resource.ResourceWithImportState = &SSLCertificateResource{}
)

// dirtiedBySSLCertificateChanges is the engine module OMV marks dirty
// whenever a SSL (or SSH) certificate is created, modified, or deleted
// (Certificates::bindListeners() in engined/module/certificates.inc).
var dirtiedBySSLCertificateChanges = []string{"certificates"}

func NewSSLCertificateResource() resource.Resource {
	return &SSLCertificateResource{}
}

// SSLCertificateResource implements the omv_ssl_certificate resource.
type SSLCertificateResource struct {
	client               *omvclient.Client
	revertOnApplyFailure bool
}

// sslCertificateResourceModel maps omv_ssl_certificate schema <-> Go.
type sslCertificateResourceModel struct {
	ID             types.String `tfsdk:"id"`
	CertificatePEM types.String `tfsdk:"certificate_pem"`
	PrivateKeyPEM  types.String `tfsdk:"private_key_pem"`
	Comment        types.String `tfsdk:"comment"`
}

func (r *SSLCertificateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ssl_certificate"
}

func (r *SSLCertificateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a SSL/TLS certificate under OpenMediaVault's System > Certificates page " +
			"(the SSL tab; SSH certificates are a separate, not-yet-implemented resource). This is a " +
			"\"bring your own certificate\" resource -- generate key material with the community " +
			"hashicorp/tls provider (or any other source) and pass its PEM output in here, rather than " +
			"expecting this resource to generate certificates itself.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "UUID assigned by OMV to this certificate.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"certificate_pem": schema.StringAttribute{
				Required:    true,
				Description: "PEM-encoded X.509 certificate.",
				Validators: []validator.String{
					fwstringvalidator.RegexMatches(
						certificatePEMPrefixRegexp,
						"must be a PEM-encoded certificate (starting with \"-----BEGIN CERTIFICATE-----\")",
					),
				},
			},
			"private_key_pem": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Sensitive: true,
				Description: "PEM-encoded private key corresponding to certificate_pem. Required by OMV " +
					"when creating a new certificate (this provider does not enforce that client-side; " +
					"OMV's own \"Private key does not exist.\" error will surface if omitted on create). " +
					"On an update, explicitly setting this to \"\" tells OMV to keep whatever private key " +
					"is already stored rather than replacing it -- otherwise this provider always sends " +
					"whatever value is in configuration/state, same as every other field. Never returned " +
					"by OMV's read API (deliberately stripped server-side), so this provider can't verify " +
					"or refresh it from the server; see this file's top doc comment.",
			},
			"comment": schema.StringAttribute{
				Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				Description: "Free-form description shown in the OMV UI.",
			},
		},
	}
}

// certificatePEMPrefixRegexp is a lightweight, client-side-only sanity
// check -- the authoritative validation is OMV's own openssl_x509_read()
// call in CertificateMgmt.set(), which this does not attempt to replicate.
var certificatePEMPrefixRegexp = regexp.MustCompile(`^\s*-----BEGIN CERTIFICATE-----`)

func (r *SSLCertificateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// sslCertificateRPCObject is the shape of a SSL certificate object as
// consumed/returned by CertificateMgmt's get/set methods. Both are safe
// to decode with this same struct (no set()-vs-get() shape divergence
// like Rsync's), EXCEPT that get() never populates PrivateKey at all --
// see this file's top doc comment.
type sslCertificateRPCObject struct {
	UUID        string `json:"uuid"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privatekey,omitempty"`
	Comment     string `json:"comment"`
}

func (r *SSLCertificateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sslCertificateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := sslCertificateRPCObject{
		UUID:        omvclient.NewObjectUUID,
		Certificate: plan.CertificatePEM.ValueString(),
		PrivateKey:  plan.PrivateKeyPEM.ValueString(),
		Comment:     plan.Comment.ValueString(),
	}

	var created sslCertificateRPCObject
	if err := r.client.Call(ctx, "CertificateMgmt", "set", obj, &created); err != nil {
		resp.Diagnostics.AddError("Error Creating SSL Certificate", err.Error())
		return
	}

	fromSSLCertificateRPCObject(&created, &plan)

	// The object now genuinely exists in OMV's config database, regardless
	// of what happens next, so resp.State is populated before the apply
	// step -- same pattern as every other resource in this provider, see
	// shared_folder_resource.go for the full rationale.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SSLCertificateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sslCertificateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var found sslCertificateRPCObject
	err := r.client.Call(ctx, "CertificateMgmt", "get", map[string]string{
		"uuid": state.ID.ValueString(),
	}, &found)
	if err != nil {
		// TODO: as in shared_folder_resource.go / rsync_job_resource.go,
		// match the specific "object does not exist" RPC error here and
		// call resp.State.RemoveResource(ctx) instead, so certificates
		// deleted out of band can be recreated by `terraform apply`.
		resp.Diagnostics.AddError("Error Reading SSL Certificate", err.Error())
		return
	}

	fromSSLCertificateRPCObject(&found, &state)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *SSLCertificateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sslCertificateResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	obj := sslCertificateRPCObject{
		UUID:        plan.ID.ValueString(),
		Certificate: plan.CertificatePEM.ValueString(),
		PrivateKey:  plan.PrivateKeyPEM.ValueString(),
		Comment:     plan.Comment.ValueString(),
	}

	var updated sslCertificateRPCObject
	if err := r.client.Call(ctx, "CertificateMgmt", "set", obj, &updated); err != nil {
		resp.Diagnostics.AddError("Error Updating SSL Certificate", err.Error())
		return
	}

	fromSSLCertificateRPCObject(&updated, &plan)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	r.applyOrHandleApplyFailure(ctx, &resp.Diagnostics)
}

func (r *SSLCertificateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sslCertificateResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.Call(ctx, "CertificateMgmt", "delete", map[string]string{
		"uuid": state.ID.ValueString(),
	}, nil)
	if err != nil {
		// CertificateMgmt.delete() calls assertIsNotReferenced() before
		// mutating anything -- if this certificate is still in use (e.g.
		// by omv_workbench_settings.ssl_certificate_id), OMV rejects the
		// delete with an RPC error, which surfaces here verbatim. Remove
		// or repoint whatever references it first.
		resp.Diagnostics.AddError("Error Deleting SSL Certificate", err.Error())
		return
	}

	// As with omv_shared_folder: the config object (and, once deployed,
	// the .crt/.key files on disk) is genuinely gone already -- delete()
	// mutates the database directly, it doesn't stage the removal. So a
	// failure here is reported as a warning, not a blocking error: state
	// removal proceeds regardless, but the operator is told the physical
	// files may still be present until a deploy runs.
	if _, err := r.client.ApplyChanges(ctx, dirtiedBySSLCertificateChanges, false); err != nil {
		resp.Diagnostics.AddWarning(
			"SSL Certificate Deleted, but Deploying the Change Failed",
			fmt.Sprintf(
				"The certificate was removed from OMV's configuration, but deploying that removal "+
					"(Config.applyChanges) failed: %s. The certificate's files under OMV's SSL "+
					"certificate directory may still be present on disk until this is resolved (retry "+
					"from the OMV web UI's pending changes panel).",
				err,
			),
		)
	}
}

func (r *SSLCertificateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// fromSSLCertificateRPCObject copies an RPC response back into the
// Terraform model. It intentionally does NOT overwrite m.PrivateKeyPEM
// from obj.PrivateKey: CertificateMgmt.get() never returns it at all
// (deliberately stripped server-side -- "It should not leave the
// system."), and even set()'s response, which does include it, isn't
// worth trusting over what was just sent for the same reason
// omv_rsync_job doesn't trust its password field's round-trip.
//
// The exception is when m.PrivateKeyPEM is null/unknown, which only
// happens right after `terraform import` (Create/Update always populate
// it from the plan before calling this). Left null forever, that would
// cause the same perpetual "will be set" diff already fixed for
// omv_shared_folder.mode and omv_rsync_job.password -- see those fixes'
// changelog entries. Falling back to the schema default ("") means an
// import followed by a plan comes back clean if the user's configuration
// also leaves it unset; if it's supposed to be managed, set it explicitly
// after importing.
func fromSSLCertificateRPCObject(obj *sslCertificateRPCObject, m *sslCertificateResourceModel) {
	m.ID = types.StringValue(obj.UUID)
	m.CertificatePEM = types.StringValue(obj.Certificate)
	m.Comment = types.StringValue(obj.Comment)
	if m.PrivateKeyPEM.IsNull() || m.PrivateKeyPEM.IsUnknown() {
		m.PrivateKeyPEM = types.StringValue("")
	}
}

// applyOrHandleApplyFailure mirrors SharedFolderResource's method of the
// same name (see shared_folder_resource.go for the full rationale),
// scoped to the modules a SSL certificate change dirties.
func (r *SSLCertificateResource) applyOrHandleApplyFailure(ctx context.Context, diags *diag.Diagnostics) {
	if _, err := r.client.ApplyChanges(ctx, dirtiedBySSLCertificateChanges, false); err != nil {
		if r.revertOnApplyFailure {
			if revertErr := r.client.RevertChanges(ctx, ""); revertErr != nil {
				diags.AddError(
					"Failed to Apply Changes, and the Automatic Revert Also Failed",
					fmt.Sprintf(
						"Applying the configuration change failed: %s. The provider then tried to revert ALL "+
							"pending configuration changes (revert_on_apply_failure = true), but that also "+
							"failed: %s. OMV's configuration database and the actual system state may now be "+
							"inconsistent -- check the OMV web UI's pending changes panel.",
						err, revertErr,
					),
				)
				return
			}
			diags.AddError(
				"Failed to Apply Changes; Reverted All Pending Changes",
				fmt.Sprintf(
					"Applying the configuration change failed: %s. Because revert_on_apply_failure = true, "+
						"the provider reverted ALL pending configuration changes on this OMV instance "+
						"(equivalent to clicking \"Undo\" in the web UI) -- including any unrelated changes "+
						"staged by other admins or tools, not just this resource's. This resource remains "+
						"recorded in Terraform state pointing at an object that may no longer reflect what "+
						"was just planned; run `terraform plan` again to reconcile.",
					err,
				),
			)
			return
		}
		diags.AddError(
			"Configuration Written, but Deploying It Failed",
			fmt.Sprintf(
				"The change was written to OMV's configuration database, but deploying it "+
					"(Config.applyChanges) failed: %s. This means the actual certificate/key files on disk "+
					"under OMV's SSL certificate directory are not yet in sync with this change. As in the "+
					"OMV web UI, the change has NOT been automatically undone -- it remains queued. Fix the "+
					"underlying issue and run `terraform apply` again, or resolve it manually (Apply/Undo) "+
					"in the OMV web UI. Set the provider's revert_on_apply_failure = true to have Terraform "+
					"automatically call Config.revertChanges instead, noting that discards ALL pending "+
					"changes instance-wide, not just this resource's.",
				err,
			),
		)
	}
}
