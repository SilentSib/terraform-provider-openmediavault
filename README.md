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

## Building

```shell
make build      # -> ./bin/terraform-provider-openmediavault
make install     # builds and installs into the local Terraform plugin cache
```

`make install` places the binary where Terraform's [development overrides]
or local filesystem mirror lookup expects it
(`~/.terraform.d/plugins/registry.terraform.io/example/openmediavault/<version>/<os_arch>/`).
Update the `HOSTNAME`/`NAMESPACE`/`NAME` values in `GNUmakefile` and
`main.go`'s `providerserver.ServeOpts.Address` once this provider has a
real registry namespace.

[development overrides]: https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers

## Using the provider

```hcl
terraform {
  required_providers {
    openmediavault = {
      source  = "example/openmediavault"
      version = "~> 0.1"
    }
  }
}

provider "openmediavault" {
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
```

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
- **Only one resource/data source pair exists.** Follow the same pattern
  (`internal/provider/shared_folder_resource.go`) to add resources for
  users, network shares (NFS/SMB), filesystems, RAID, scheduled jobs,
  etc. -- and re-verify each one's exact RPC method/field names and
  dirtied-modules list against that service's `.inc` file the same way,
  rather than assuming they follow `ShareMgmt`'s pattern exactly (some
  services don't use plain `get`/`set`/`delete`, e.g. `Config` itself).
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
