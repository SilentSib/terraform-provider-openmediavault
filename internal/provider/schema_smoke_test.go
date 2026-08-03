package provider

import (
	"context"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// TestResourceTypeNamesMatchOMVPrefix is a protocol-level smoke test
// guarding against the exact bug reported in production use: the
// provider's Metadata() TypeName must be "omv" so the resource types it
// actually serves over the wire are "omv_shared_folder"/"omv_rsync_job",
// matching every example/README snippet. If TypeName drifts back to
// "openmediavault" (or anything else), Terraform configs written against
// the docs would fail at `terraform init`/`plan` with a provider
// resolution error, NOT a schema error, which is easy to misdiagnose --
// see the regression this test is named after.
func TestResourceTypeNamesMatchOMVPrefix(t *testing.T) {
	srv, err := testAccProtoV6ProviderFactories["omv"]()
	if err != nil {
		t.Fatalf("failed to build provider server: %v", err)
	}

	resp, err := srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatalf("GetProviderSchema failed: %v", err)
	}
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("GetProviderSchema returned an error diagnostic: %s: %s", d.Summary, d.Detail)
		}
	}

	var got []string
	for name := range resp.ResourceSchemas {
		got = append(got, name)
	}
	sort.Strings(got)

	want := []string{"omv_rsync_job", "omv_shared_folder", "omv_ssh_certificate", "omv_ssl_certificate", "omv_workbench_settings"}
	if len(got) != len(want) {
		t.Fatalf("unexpected resource type set: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unexpected resource type set: got %v, want %v", got, want)
			break
		}
	}

	for name := range resp.DataSourceSchemas {
		if name != "omv_shared_folder" {
			t.Errorf("unexpected data source type: %s", name)
		}
	}
}
