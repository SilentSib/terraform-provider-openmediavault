# Changelog

## Unreleased

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
