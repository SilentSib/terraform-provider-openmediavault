# Changelog

## Unreleased

- **Fixed:** `Client.Login()` decoded `Session.login`'s response as a bare
  `bool`, which fails against a real OMV instance with `json: cannot
  unmarshal object into Go value of type bool` on every login attempt.
  The actual response (verified against
  `var/www/openmediavault/rpc/session.inc` in the OMV 8.5.5 source) is an
  object with a `status` field (`"authenticated"` or, if the account has
  MFA enabled, `"challengeRequired"` plus challenge details), not a
  boolean -- this client's earlier assumption came from documentation that
  turned out to be stale for current OMV versions, not from the source.
  Fixed `Login()` to parse the real shape and to return a clear error
  (rather than a confusing decode failure) when an account has MFA
  enabled, since a second `Session.verify` challenge-response step isn't
  implemented. Added regression tests (`TestLoginResponseShapes`) pinned
  to all three cases -- authenticated, challenge-required, and the
  original bare-boolean shape now correctly rejected -- and fixed the
  hardcoded `{"response": true}` login mocks in the other test files that
  had been masking this.

- **Fixed:** the provider's `Metadata().TypeName` was `"openmediavault"`,
  so the resource types it actually served over the wire were
  `openmediavault_shared_folder`/`openmediavault_rsync_job` -- not
  `omv_shared_folder`/`omv_rsync_job` as every example, the README, and
  the acceptance tests assumed. Combined with a `required_providers` local
  name that also didn't match the `omv_` prefix used in resource blocks,
  this produced Terraform's generic "provider registry.terraform.io does
  not have a provider named registry.terraform.io/hashicorp/omv" error
  for anyone following the docs as written, since Terraform infers the
  local name to look up from a resource type's prefix and falls back to
  assuming `hashicorp/<prefix>` when nothing matches. Fixed the
  `TypeName`, the README/example `required_providers`/`provider` blocks,
  and the acceptance test harness's provider factory key, all to `omv`;
  added a protocol-level test (`TestResourceTypeNamesMatchOMVPrefix`)
  asserting the served resource type names directly so this can't silently
  regress again. Also expanded the README's "Building" section with a
  corrected, tested `dev_overrides` workflow for running an unpublished
  provider locally.

- Initial provider scaffolding: provider configuration/auth against OMV's
  JSON-RPC API, minimum-version (OMV >= 8) enforcement, and a template
  `omv_shared_folder` resource/data source pair demonstrating the CRUD +
  import pattern to follow for further resources.
- Verified `omv_shared_folder`'s RPC calls against the actual OMV 8.5.5
  source: corrected the service methods to `ShareMgmt.get`/`set`/`delete`
  (previously guessed as `getSharedFolder`/etc.), fixed the "new object"
  UUID sentinel to the real literal value, made `comment` always sent
  (required at the RPC layer), and added the `mode` attribute with its
  actual enum of valid values.
- Added `Config.applyChanges`/`revertChanges`/`isDirty` support to the
  client and wired `omv_shared_folder`'s Create/Update/Delete to deploy
  pending changes after each mutation. Added the provider-level
  `revert_on_apply_failure` option (default off) controlling whether a
  failed deploy triggers an automatic, instance-wide revert -- verified
  against source that the OMV web UI does *not* do this by default.
- Added `omv_rsync_job`, managing OMV's scheduled rsync jobs (`Rsync` RPC
  service). Verified its RPC service/method/field names against the OMV
  8.5.5 source the same way as `omv_shared_folder`. Reuses the
  apply/revert-on-failure handling introduced above, with messaging
  specific to rsync jobs' deploy step actually writing cron
  jobs/scripts to disk (unlike shared folders' no-op module deploy).
