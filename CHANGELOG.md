# Changelog

## Unreleased

- Added `omv_workbench_settings`, managing OMV's System > Workbench >
  Settings page (web UI port, session auto-logout timeout, SSL/TLS) via
  the `WebGui` RPC service's `getSettings`/`setSettings` methods,
  verified against the OMV 8.5.5 source. Unlike every other resource in
  this provider, the underlying `conf.webadmin` config object is a
  genuine singleton (`"iterable": false`, no UUID), so this resource uses
  a fixed literal Terraform id (`"settings"`) and has no real `Delete`
  (there's no RPC to reset it; removing it from Terraform state just
  stops managing it). Added a best-effort `ValidateConfig` mirroring the
  web UI's own port/SSL-certificate cross-field checks (skipped when
  either side is still unknown, to avoid false positives against
  not-yet-defaulted values). Documented prominently, in both the
  resource/attribute descriptions and the README, that this resource
  controls how every client -- including this provider's own connection
  -- reaches OMV, so changing port/SSL settings can require updating the
  provider block itself and can make a successful apply look like a
  connection failure if the web server restarts mid-response.

- **Fixed:** `omv_shared_folder`'s `mode` (and, by the same root cause,
  `omv_rsync_job`'s `password`) showed a spurious diff on every single
  `terraform plan` after `terraform import`, even when the configured
  value already matched reality. Cause: both attributes are
  Optional+Computed values that this provider deliberately never
  refreshes from a normal Read -- `ShareMgmt.get()` doesn't return `mode`
  at all, and `password` is intentionally not read back from
  `Rsync.get()`'s response so an out-of-band change on the server doesn't
  fight with a plan that's changing it. That "leave it as whatever's
  already in state" logic is correct for a normal refresh (state already
  holds a real value written by a prior Create/Update), but
  `terraform import` only populates `id`, so state has no value at all to
  leave alone, and it stayed null forever. Fixed by having `Read()`
  (`omv_shared_folder`) and `fromRPCObject()` (`omv_rsync_job`, shared by
  Read/Create/Update) fall back to the schema default specifically when
  the existing value is null/unknown -- normal refreshes are unaffected,
  since state already has a real value by then. Added tests exercising
  the actual `Read()` method end-to-end (not just the internal helpers)
  against a fake server shaped like the real one, reproducing the exact
  reported scenario.

- **Fixed (audit pass):** went through every remaining RPC call this
  client makes line-by-line against the OMV 8.5.5 source, prompted by two
  prior bugs that both came from trusting assumptions instead of the
  source. Found and fixed three more real issues:
  - `Rsync.set()`'s response is a DIFFERENT shape than `Rsync.get()`'s:
    `set()` doesn't convert the stored comma-separated
    minute/hour/dayofmonth/month/dayofweek strings into arrays (only
    `get()` does), and never populates the flat
    srcsharedfolderref/srcuri/destsharedfolderref/desturi convenience
    fields (only nested "src"/"dest" objects). Decoding `set()`'s
    response directly into the same struct used for `get()` -- which
    `omv_rsync_job`'s Create/Update did -- would have crashed on every
    real create/update with the same "cannot unmarshal string" class of
    error already fixed twice elsewhere, and even if it hadn't crashed,
    would have silently wiped the src/dest fields from state. Fixed by
    having Create/Update decode only the UUID out of `set()`'s response,
    then re-fetch the canonical object via `get()` (the same call `Read`
    already makes) before populating state. Added regression tests
    reproducing both the realistic response shapes and a negative-control
    test confirming the old approach really would have failed.
  - `ShareMgmt.set()` DOES echo `mode` back in its response (verified via
    the exact source line that appends it before returning) -- a doc
    comment claiming otherwise was simply wrong, though harmless since
    the code already didn't rely on it. Now `omv_shared_folder`'s
    Create/Update read the real value back, and the schema description
    is more precise about a genuine underlying-API quirk found in the
    same code path: changing `mode` on an *existing* shared folder is
    accepted and echoed back by OMV, but does not actually chmod the
    directory again (chmod only runs the first time the directory is
    created) -- previously undocumented.
  - The `name` attribute's regex validator excluded the space character
    entirely, incorrectly rejecting valid share names with internal
    spaces (e.g. "My Shared Folder"). The actual MS-FSCC validator in
    `datamodel/schema.inc` only forbids *leading/trailing* space --
    its own comment says so explicitly. Root cause: the PHP source uses
    lookaround (`(?![ ])...(?<! )`), which Go's RE2 engine doesn't
    support, and the earlier hand-rewrite lost that distinction when
    working around it. Fixed and added a test matrix covering every
    forbidden character plus internal-space cases.
  - Also verified (no changes needed): `Config.isDirty`/`applyChanges`/
    `revertChanges`'s exact return shapes, `ShareMgmt.delete()`'s and
    `Rsync.delete()`'s return shapes (both safely discarded already), and
    every RPC request param's type against its datamodel schema
    (`rpc.rsync.json`, `rpc.sharemgmt.json`, `rpc.common.json`,
    `rpc.config.json`).

- **Fixed:** `SystemInformation` modeled `memTotal` as `int64` and
  `cpuUsage` (a field that doesn't actually exist; the real name is
  `cpuUtilization`) as `int`, which crashed decoding
  `System.getInformation`'s response with `json: cannot unmarshal string
  into Go struct field SystemInformation.memTotal of type int64`.
  `engined/rpc/system.inc`'s own doc comment explains why: "all numbers
  that might be > 4GiB are returned as strings to keep the 32bit
  compatibility" -- several numeric-looking fields are JSON strings or
  JSON numbers depending on the runtime value (e.g. how much RAM the
  target system has), so decoding them into a fixed numeric Go type is
  inherently fragile. Trimmed `SystemInformation` down to the two fields
  this provider actually uses (`hostname`, `version`, both always
  strings) and documented the >4GiB quirk so future fields are added with
  a tolerant type. Also discovered and handled: `version` is formatted as
  `"<dpkg version> (<release codename>)"`, e.g. `"8.5.5-1 (Shaitung)"`,
  not just the bare version (harmless for `CheckMinVersion`'s parsing,
  which already only looks at digits before the first "."); and most
  fields including `version` are only present at all when the
  authenticated account has the administrator role, now reported as a
  distinct, clearer error instead of an opaque parse failure.

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
