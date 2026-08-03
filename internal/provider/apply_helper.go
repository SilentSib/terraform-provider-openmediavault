package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/example/terraform-provider-openmediavault/internal/omvclient"
)

// applyOrHandleApplyFailure calls Config.applyChanges scoped to modules,
// and on failure either adds a blocking error diagnostic (default) or, if
// revertOnApplyFailure is set, also calls Config.revertChanges. This is
// shared by every resource in this provider that mutates OMV's config
// database (they were previously near-identical per-resource copies of
// this same method; see the git history if you need the last diverged
// version for reference).
//
// On failure it either:
//   - (default) adds a blocking error diagnostic and leaves the change
//     queued in OMV, matching what the OMV web UI itself does when its
//     "Apply" button's RPC call fails -- it does NOT auto-revert, only its
//     separate, explicit "Undo" button does. The object each resource
//     manages was already written to OMV's config database (callers
//     resp.State.Set before calling this), so it stays present -- just
//     "dirty" -- in both OMV and Terraform state, consistent with the web
//     UI leaving it in the pending changes list for the operator to retry
//     or undo by hand.
//   - (opt-in via the provider's revert_on_apply_failure) also calls
//     Config.revertChanges, mirroring a manual click of "Undo". Since that
//     RPC discards the ENTIRE pending-changes queue instance-wide, not
//     just this resource's change, the error message says so explicitly.
//
// IMPORTANT DISTINCTION this function makes that a naive "if err != nil"
// check would miss: Config.applyChanges is proxied by rpc.php straight to
// the long-running omv-engined daemon (see omvclient.Config.DeployTimeout's
// doc comment), which keeps running the underlying Salt deployment to
// completion regardless of whether this provider's HTTP client is still
// waiting for the response. A context-deadline-exceeded error here
// therefore does NOT necessarily mean the deploy failed -- it may well
// have completed successfully after this client gave up waiting for
// confirmation (this is a real scenario this project hit in production;
// see CHANGELOG.md for the incident this distinction was added in
// response to). The error message for that case says so explicitly,
// rather than implying an authoritative failure the way the generic
// message does.
func applyOrHandleApplyFailure(ctx context.Context, client *omvclient.Client, revertOnApplyFailure bool, modules []string, diags *diag.Diagnostics) {
	_, err := client.ApplyChanges(ctx, modules, false)
	if err == nil {
		return
	}

	if isLikelyClientSideTimeout(err) {
		diags.AddError(
			"Configuration Written, but Confirming the Deploy Timed Out (Deploy Likely Still Succeeded)",
			fmt.Sprintf(
				"The change was written to OMV's configuration database, and the follow-up deploy step "+
					"(Config.applyChanges) was sent, but this provider timed out waiting for OMV's response: "+
					"%s. This is NOT necessarily a real failure: Config.applyChanges runs as a real Salt "+
					"deployment inside OMV's separate omv-engined daemon, which keeps running to completion "+
					"independently of whether this client is still waiting -- a timeout here often just means "+
					"the deploy was slower than expected (e.g. on constrained hardware) and likely finished "+
					"successfully after this provider stopped waiting. Check the OMV web UI's pending changes "+
					"indicator to confirm: if it shows nothing pending for this change, re-running `terraform "+
					"apply` should now complete quickly and cleanly. If you see this often, raise the "+
					"provider's deploy_timeout_seconds (default 300).",
				err,
			),
		)
		return
	}

	if revertOnApplyFailure {
		if revertErr := client.RevertChanges(ctx, ""); revertErr != nil {
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
				"(Config.applyChanges) failed: %s. As in the OMV web UI, the change has NOT been "+
				"automatically undone -- it remains queued. Fix the underlying issue and run "+
				"`terraform apply` again, or resolve it manually (Apply/Undo) in the OMV web UI. Set "+
				"the provider's revert_on_apply_failure = true to have Terraform automatically call "+
				"Config.revertChanges instead, noting that discards ALL pending changes instance-wide, "+
				"not just this resource's.",
			err,
		),
	)
}

// isLikelyClientSideTimeout reports whether err represents this client
// giving up waiting for a response (a context deadline or a lower-level
// network timeout), as opposed to an actual error returned by OMV (an
// *rpcError, or any other error). Deliberately conservative: only a
// genuine deadline/timeout is treated as ambiguous-not-necessarily-failed;
// everything else (including connection refused, TLS errors, DNS
// failures, and RPC-level errors from OMV itself) is treated as a real
// failure, since only a timeout has the specific "the other side might
// still be working" property this distinction is about.
func isLikelyClientSideTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) {
		return timeoutErr.Timeout()
	}
	return false
}
