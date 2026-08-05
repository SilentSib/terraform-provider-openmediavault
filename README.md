# Terraform Provider for OpenMediaVault

A [Terraform](https://www.terraform.io) provider for managing
[OpenMediaVault](https://www.openmediavault.org/) (OMV) NAS instances
through OMV's JSON-RPC API.

> **Status:** early scaffolding. The provider authenticates, enforces a
> minimum OMV version, and ships one worked-example resource/data source
> (`omv_shared_folder`) as a template — see [Status &
> caveats](#status--caveats) before relying on it.

**Requires OpenMediaVault 8 or newer.** The provider checks the connected
instance's reported version during `Configure` and fails fast if it's
below 8.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.0
- [Go](https://go.dev/doc/install) >= 1.22 (for building the provider)
- An OpenMediaVault >= 8 instance reachable over HTTP(S), with an account
  that has RPC access (typically the `admin` account or a user in the
  `admin` role)
- **That account must NOT have multi-factor authentication enabled.**
  OMV's login flow can require a second `Session.verify` challenge step
  (TOTP, etc.) that this provider doesn't implement; logging in with an
  MFA-enabled account fails with a clear "requires additional multi-factor
  authentication" error rather than hanging or silently failing. Use a
  separate service account with MFA disabled for Terraform automation.

## Building

```shell
make build      # -> ./bin/terraform-provider-openmediavault
```

This provider isn't published to any registry, so `terraform init` can't
just download it -- you need to point Terraform at the binary you just
built. Two ways to do that:

### Option A: `dev_overrides` (recommended for iterating locally)

Add to `~/.terraformrc` (create it if it doesn't exist):

```hcl
provider_installation {
  dev_overrides {
    "example/openmediavault" = "/absolute/path/to/terraform-provider-openmediavault/bin"
  }
  direct {}
}
```

The key is the provider's `source` address (matching `main.go`'s
`providerserver.ServeOpts.Address` and the `source` in your
`required_providers` block), not the `omv` local name. With this in place,
`terraform plan`/`apply` use the binary in that directory directly --
`terraform init` isn't even needed for this provider, and there's no
version/checksum matching to keep in sync as you rebuild. Terraform prints
a warning on every run reminding you an override is active; that's
expected. Remove the `dev_overrides` block (or comment it out) once you no
longer want to use your local build.

### Option B: filesystem mirror (`make install`)

```shell
make install     # builds and installs into a local Terraform plugin mirror
```

This places the binary where Terraform's [implied local filesystem
mirror] lookup expects it
(`~/.terraform.d/plugins/registry.terraform.io/example/openmediavault/<version>/<os_arch>/`)
and works with a normal `terraform init`, but -- unlike `dev_overrides` --
the version in `GNUmakefile`'s `VERSION` variable must satisfy your
config's `required_providers` version constraint, and you need to rerun
`terraform init` (with `-upgrade` if the version string didn't change)
after every rebuild.

Update the `HOSTNAME`/`NAMESPACE`/`NAME` values in `GNUmakefile` and
`main.go`'s `providerserver.ServeOpts.Address` together, and keep them
consistent, if you rename this provider or give it a real registry
namespace.

[implied local filesystem mirror]: https://developer.hashicorp.com/terraform/cli/config/config-file#implied-local-mirror-directories

## Using the provider

```hcl
terraform {
  required_providers {
    omv = {
      source  = "example/openmediavault"
      version = "~> 0.1"
    }
  }
}

provider "omv" {
  host     = "nas.example.lan"
  username = "admin"
  # password can also come from the OMV_PASSWORD environment variable
}

resource "omv_shared_folder" "media" {
  name           = "media"
  mount_point_id = "fs-uuid-of-the-target-filesystem"
  relative_path  = "media/"
  comment        = "Managed by Terraform"
}

resource "omv_rsync_job" "nightly_backup" {
  type                  = "local"
  enabled               = true
  comment               = "Nightly mirror to backup volume"
  src_shared_folder_id  = omv_shared_folder.media.id
  dest_shared_folder_id = "fs-uuid-of-the-destination-shared-folder"
  hour                  = ["2"]
  minute                = ["0"]
}

# Singleton -- there is exactly one of these per OMV instance. See its
# description for the operational warning about changing your own
# connection settings.
resource "omv_workbench_settings" "this" {
  port                = 80
  auto_logout_minutes = 15
}

# Bring your own certificate (e.g. from the hashicorp/tls provider) --
# see examples/resources/omv_ssl_certificate for a full example.
resource "omv_ssl_certificate" "this" {
  certificate_pem = file("cert.pem")
  private_key_pem = file("key.pem")
  comment          = "Managed by Terraform"
}

# See examples/resources/omv_ssh_certificate for the format constraints
# (OMV rejects ECDSA public keys and PKCS8/EC private keys).
resource "omv_ssh_certificate" "this" {
  public_key_openssh = file("id_ed25519.pub")
  private_key_pem     = file("id_ed25519") # must be OpenSSH- or RSA-formatted PEM
}

resource "omv_smb_share" "media" {
  enabled          = true
  shared_folder_id = omv_shared_folder.media.id
  comment          = "Media library"
  recycle_bin      = true
}

# NFS is many-to-one: multiple omv_nfs_share resources can reference the
# same shared_folder_id, one per client/network needing different rules.
resource "omv_nfs_share" "media_lan" {
  shared_folder_id = omv_shared_folder.media.id
  client            = "192.168.1.0/24"
  options           = "ro"
}
```

The `required_providers` key (`omv` above) and the `provider` block's label
must both match the prefix used by this provider's resource/data source
types (`omv_shared_folder`, `omv_rsync_job`, ...) -- Terraform infers which
provider a resource belongs to from the part of its type before the first
`_`, and only consults `source`/`version` for whichever `required_providers`
entry has that same key. If they don't match (or the block is missing
entirely), Terraform falls back to assuming `registry.terraform.io/hashicorp/omv`
and fails with something like:

```
Error: Failed to query available provider packages
Could not retrieve the list of available versions for provider hashicorp/omv
```

even though nothing in your configuration mentions `hashicorp/omv` by name.


### Provider configuration

| Attribute               | Environment variable       | Default   | Description                                          |
| ------------------------ | --------------------------- | --------- | ----------------------------------------------------- |
| `host`                   | `OMV_HOST`                  | —         | Hostname or IP of the OMV instance. Required.          |
| `port`                   | `OMV_PORT`                  | 443 / 80  | TCP port of the OMV web UI.                            |
| `scheme`                 | `OMV_SCHEME`                | `https`   | `https` or `http`.                                     |
| `username`               | `OMV_USERNAME`               | —         | OMV account to authenticate as. Required.               |
| `password`               | `OMV_PASSWORD`               | —         | Password for that account. Required, sensitive.         |
| `insecure_skip_verify`   | `OMV_INSECURE_SKIP_VERIFY`  | `false`   | Skip TLS verification (OMV's default cert is self-signed). |
| `revert_on_apply_failure`| —                            | `false`   | Auto-call `Config.revertChanges` (mirrors the web UI's "Undo") when a deploy fails. Instance-wide blast radius -- see [Status & caveats](#status--caveats). |
| `request_timeout_seconds`| —                            | `60`      | Timeout for ordinary RPC calls (login, most get/set/delete). |
| `deploy_timeout_seconds` | —                            | `300`     | Timeout specifically for `Config.applyChanges`/`revertChanges`, which run a real (potentially slow) Salt deployment, not a quick database write -- see [Status & caveats](#status--caveats). Raise this on constrained hardware. |

See `examples/provider/provider.tf` for a fuller example, and
`examples/resources/` / `examples/data-sources/` for per-resource examples.

## How it talks to OMV

OMV doesn't expose a REST API; the web UI (and this provider) drive it
through a JSON-RPC endpoint at `https://<host>/rpc.php`. Every call is a
POST of `{"service": ..., "method": ..., "params": ...}` and gets back
`{"response": ..., "error": ...}`. Authentication is session-cookie based:
the client first calls `session.login` with a username/password and
reuses the resulting cookie for every subsequent call
(`internal/omvclient/client.go`).

## Status & caveats

This repository is groundwork, not a finished provider:

- **A "deploy failed" error may not mean what it says -- read this before
  assuming a failed apply didn't take effect.** `Config.applyChanges` is
  proxied by `rpc.php` straight to the long-running `omv-engined` daemon
  (verified via `usr/share/php/openmediavault/rpc/proxy/json.inc`), which
  then runs a real Salt deployment (`omv-salt deploy run <module>`) --
  this can legitimately take longer than a simple database read/write,
  especially on constrained hardware like a Raspberry Pi, and the daemon
  keeps running it to completion regardless of whether this provider's
  HTTP client is still waiting for the response. A client-side timeout
  here is therefore ambiguous -- the deploy may well have completed
  successfully after this provider gave up waiting -- and this provider
  now detects that specific case and reports it distinctly ("Configuration
  Written, but Confirming the Deploy Timed Out (Deploy Likely Still
  Succeeded)") rather than as an unambiguous failure. If you see that
  error, check the OMV web UI's pending changes indicator before assuming
  anything is wrong; a plain `terraform apply` retry should then complete
  quickly. The provider's `deploy_timeout_seconds` (default 300) controls
  how long it waits before giving up.
- **`omv_shared_folder`'s RPC calls (service `ShareMgmt`, methods `get`/
  `set`/`delete`) were verified against the real OMV 8.5.5 source**
  (`usr/share/openmediavault/engined/rpc/sharemgmt.inc` and the
  `rpc.sharemgmt`/`conf.system.sharedfolder` datamodels, from
  https://github.com/openmediavault/openmediavault, commit `96cd9aa`), not
  just guessed. Notably: the "generate a new UUID" sentinel is the fixed
  literal `fa4b1c66-ef79-11e5-87a0-0002b3a176b4`
  (`omvclient.NewObjectUUID`), not a word like `"newuuid"`; `comment` is a
  *required* RPC param even though it's optional in the UI; `mode` is
  restricted to `700`/`750`/`755`/`770`/`775`/`777`. If you're targeting a
  different OMV version, diff `sharemgmt.inc` against that check before
  trusting it further.
- **Config apply/revert semantics were also verified against source**
  (`engined/rpc/config.inc` and the web UI's
  `apply-config-panel.component.ts`). Two things worth knowing before you
  extend this pattern to other resources:
  1. OMV's pending-changes queue (`Config.applyChanges` /
     `Config.revertChanges`) is **instance-wide**, not scoped to a single
     object -- there's no way to apply or revert just the one thing a
     resource changed.
  2. The real OMV web UI does **not** auto-revert on a failed apply: its
     "Apply" button just surfaces an error and leaves the change queued;
     "Undo" is a separate, explicit, always-manual action.

  `omv_shared_folder`'s Create/Update therefore: write the change, save it
  to Terraform state (so a failed deploy doesn't lose track of an object
  that really does exist in OMV), then call `Config.applyChanges` scoped
  to the modules a shared-folder change dirties (`sharedfolders`,
  `systemd`). On failure it reports a blocking error and leaves the change
  queued in OMV -- matching the web UI's default behavior -- rather than
  silently auto-reverting (which could discard other admins'/tools'
  unrelated pending changes). Set the provider's `revert_on_apply_failure
  = true` if you want Terraform to instead call `Config.revertChanges`
  automatically on a failed apply (i.e. mirror clicking "Undo"), with that
  instance-wide blast radius clearly spelled out in the resulting error
  message. Delete works differently: `ShareMgmt.delete()` removes the
  config object (and, recursively, its directory) immediately rather than
  staging it, so a failed post-delete `applyChanges` there is reported as
  a non-blocking warning instead, since the object is genuinely already
  gone.
- **Seven resources exist so far** (`omv_shared_folder`, `omv_rsync_job`,
  `omv_workbench_settings`, `omv_ssl_certificate`, `omv_ssh_certificate`,
  `omv_smb_share`, `omv_nfs_share`) plus one data source
  (`omv_shared_folder`). Follow the same pattern to add resources for
  users, filesystems, RAID, etc. -- and re-verify each one's exact RPC
  method/field names and dirtied-modules list against that service's
  `.inc` file the same way, rather than assuming they follow
  `ShareMgmt`/`Rsync`'s pattern exactly (some services don't use plain
  `get`/`set`/`delete`, e.g. `Config` itself).
- **`omv_nfs_share` works meaningfully differently from `omv_smb_share`,
  despite the similar name.** Verified against the OMV 8.5.5 source:
  - NFS is many-to-one where SMB is one-to-one: a NFS "share" is really
    one `(client, options)` export rule, and OMV places no uniqueness
    constraint on `shared_folder_id` (no `assertIsUnique`, unlike
    `ShareMgmt`/`Smb`) -- create multiple `omv_nfs_share` resources
    against the same shared folder, one per client/network.
  - Creating the *first* NFS share for a shared folder makes OMV silently
    create a bind mount (binding the shared folder's directory into the
    NFS export directory) via an internal call to the `FsTab` RPC
    service, and sets `mntentref` to point at it -- overwriting whatever
    was sent, but only on create. `mount_entry_id` is therefore modeled
    as purely Computed: the sentinel `omvclient.NewObjectUUID` is sent on
    create (exactly matching the OMV web UI's own hidden form field,
    verified by reading the Angular component), and the real value is
    always resent verbatim on update, since `setShare()` does not
    re-derive it except on a brand-new object.
  - Because of that bind-mount machinery, deploying a share change can
    require the `nfs`, `fstab` (confirmed via `engined/module/fstab.inc`
    -- this is what actually performs the bind mount on disk), and
    `zeroconf` modules, not just `nfs`; this resource always requests all
    three, relying on `Config.applyChanges` only acting on whichever are
    actually dirty (verified from source).
  - `extra_options`' pattern requires **at least one token** -- an empty
    string is genuinely rejected by OMV's own validation (verified with
    `php -r` against the actual pattern, not assumed from reading the
    regex), so this resource defaults it to `"subtree_check,insecure"`
    (matching the OMV web UI's own suggested value) rather than empty.
    The same check also found the pattern's key=value syntax doesn't
    support hyphens in values, so hyphenated values (e.g. a dashed UUID)
    are rejected by OMV itself -- both are documented on the attribute
    and covered by tests that were themselves corrected after initially
    assuming the wrong behavior.
- **`omv_smb_share`'s RPC calls (service `Smb`, methods `getShare`/
  `setShare`/`deleteShare`) were verified against the OMV 8.5.5 source**,
  and unlike `Rsync`, `getShare()`/`setShare()` have no shape divergence
  -- both simply return `$object->getAssoc()`. Two things worth knowing:
  (1) at most one share may reference a given `shared_folder_id` -- OMV
  enforces this server-side (`assertIsUnique`) and rejects a second one
  with a clear error; (2) four fields
  (`time_machine_max_size`/`transport_encryption`/`follow_symlinks`/
  `wide_links`) are absent from the RPC's own params schema but are still
  fully settable -- confirmed by reading OMV's JSON schema validator
  directly (it only checks properties it declares, never rejects extras)
  and that the underlying config object is validated against the fuller
  config datamodel, not the narrower RPC schema. Also note
  `recycle_bin_max_size_bytes`/`recycle_bin_retention_days` are named for
  their actual units (bytes / days) rather than the RPC's unit-less
  `recyclemaxsize`/`recyclemaxage`, confirmed from the web UI's form
  definition since the datamodel itself doesn't say.
- **`omv_ssh_certificate` accepts a narrower set of key formats than
  "any valid SSH key."** Verified against OMV's own format validators
  (`\OMV\Ssh\PublicKey::isOpenSSH()` and the `sshprivkey-pem` schema
  format in `datamodel/schema.inc`): `public_key_openssh` only accepts
  `ssh-rsa`, `ssh-ed25519`, or `sk-ssh-ed25519@openssh.com` -- ECDSA
  (`ecdsa-sha2-*`) and DSA (`ssh-dss`) keys are rejected. `private_key_pem`
  only accepts PEM headed `-----BEGIN OPENSSH PRIVATE KEY-----` or
  `-----BEGIN RSA PRIVATE KEY-----` -- notably NOT the generic PKCS8
  `-----BEGIN PRIVATE KEY-----` that `hashicorp/tls`'s `tls_private_key.
  private_key_pem` produces for ed25519/ECDSA keys, and not
  `-----BEGIN EC PRIVATE KEY-----` either. Use `private_key_openssh`
  from `tls_private_key` for non-RSA algorithms -- see
  `examples/resources/omv_ssh_certificate`. Both formats are validated
  client-side (mirroring OMV's exact regexes, tested against the same
  cases) so a mismatch is caught at `plan` time rather than as an opaque
  RPC error.
- **`omv_ssl_certificate` deliberately doesn't generate certificates.**
  OMV's `CertificateMgmt.create()` RPC can generate a self-signed
  cert+key server-side, but this resource only wraps `get`/`set`/
  `delete` (bring-your-own PEM content) -- pair it with the community
  `hashicorp/tls` provider's `tls_private_key`/`tls_self_signed_cert` (or
  any other source of PEM material, e.g. ACME) instead, matching how
  most other Terraform providers' certificate resources work. Also note:
  `CertificateMgmt.get()` deliberately never returns the private key
  (stripped server-side, "should not leave the system"), so
  `private_key_pem` follows the same never-refreshed-from-a-normal-Read,
  defaults-to-empty-after-import pattern as `omv_rsync_job.password`.
- **`omv_workbench_settings` is a singleton** (OMV's `conf.webadmin`
  config object has `"iterable": false` -- there's exactly one per
  instance, no UUID at all), so it uses a fixed literal Terraform id
  (`"settings"`) and has no real `Delete` (removing it from Terraform
  state doesn't reset OMV's web UI settings; there's no RPC to do that).
  More importantly: **this resource controls how every client, including
  this provider's own connection, reaches OMV.** Changing `port`,
  `enable_ssl`, `ssl_port`, or `force_ssl_only` can require updating the
  `omv` provider block's own `port`/`scheme` before the next
  `plan`/`apply` will connect, and the very `apply` that changes them may
  report a connection error even though the change succeeded, if nginx
  restarts before the HTTP response finishes. Test against a disposable
  instance before using this on anything you can't afford to get locked
  out of.
- **`omv_rsync_job`'s RPC calls (service `Rsync`, methods `get`/`set`/
  `delete`) were likewise verified against the OMV 8.5.5 source**
  (`engined/rpc/rsync.inc`, `engined/module/rsync.inc`, and the
  `rpc.rsync`/`conf.service.rsync.job` datamodels). Notable details baked
  into the resource: the RPC layer requires every one of ~30 fields to be
  present on `set()` even when irrelevant to the chosen `type`/`mode`
  (left at their zero value in that case, matching the web UI's own
  form); `minute`/`hour`/`dayofmonth`/`month`/`dayofweek` are arrays at
  the RPC layer despite being stored as comma-separated strings
  internally; and unlike shared folders, the `rsync` module's deploy step
  is **not** a no-op -- it's what actually writes the cron job and
  wrapper script to disk, so a failed `Config.applyChanges` after a
  rsync-job change is more consequential (a stale job could keep running
  on its old schedule) and is called out more pointedly in that
  resource's error/warning messages. Password auth stores the password
  in OMV's config database in plaintext (matching OMV's own behavior);
  prefer `authentication = "pubkey"` where possible.
- **`go.mod` contains a number of `replace` directives** redirecting
  `golang.org/x/*`, `google.golang.org/*`, and a few `gopkg.in/*` modules
  to their GitHub mirrors. These were only needed because the sandbox
  this scaffolding was generated in couldn't reach `golang.org` /
  `google.golang.org` / `gopkg.in` directly. If your normal build
  environment has unrestricted module proxy access, you can likely delete
  them and run `go mod tidy` to get the canonical module paths back --
  test that this still resolves and builds before merging.
- **No `docs/` directory yet.** Run `make generate` (needs network access
  to fetch `tfplugindocs`) to generate registry-ready docs from the schema
  descriptions and `examples/`.

## Testing

```shell
make test      # unit tests, no network access needed
make testacc   # acceptance tests against a REAL OMV instance - see below
```

Acceptance tests run actual `terraform apply`/`destroy` cycles against a
live OMV instance and can create/modify/delete real objects on it. Point
them at a disposable OMV VM, not production storage. Required environment
variables: `OMV_HOST`, `OMV_USERNAME`, `OMV_PASSWORD`, `TF_ACC=1`
(`make testacc` sets `TF_ACC` for you).

## Releasing

Tagging a `vX.Y.Z` commit triggers `.github/workflows/release.yml`, which
runs [GoReleaser](https://goreleaser.com/) (`.goreleaser.yml`) to build
binaries for linux/darwin/windows/freebsd, sign the checksums with GPG
(`GPG_PRIVATE_KEY`/`PASSPHRASE` secrets), and publish a GitHub release
that the Terraform Registry can index. See HashiCorp's [publishing
guide](https://developer.hashicorp.com/terraform/registry/providers/publishing)
for the registry-side setup (GPG key upload, repository naming
convention, etc.) required before this will show up on
registry.terraform.io.

## License

[Mozilla Public License 2.0](./LICENSE), matching the convention used by
HashiCorp's official Terraform providers.
