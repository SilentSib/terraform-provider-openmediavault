default: build

.PHONY: build install lint fmt vet test testacc generate clean

build:
	go build -o bin/terraform-provider-openmediavault .

# Installs the provider into the local Terraform plugin cache so
# `terraform init` picks it up without a registry. Adjust OS_ARCH if you
# are not on linux/amd64.
OS_ARCH ?= linux_amd64
VERSION ?= 0.1.0
PLUGIN_DIR := ~/.terraform.d/plugins/registry.terraform.io/example/openmediavault/$(VERSION)/$(OS_ARCH)

install: build
	mkdir -p $(PLUGIN_DIR)
	cp bin/terraform-provider-openmediavault $(PLUGIN_DIR)/terraform-provider-openmediavault_v$(VERSION)

fmt:
	gofmt -s -w .
	go run golang.org/x/tools/cmd/goimports@latest -w .

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/cmd/golangci-lint@latest run ./...

test:
	go test -count=1 -v ./...

# Acceptance tests exercise real RPC calls against a live OMV >= 8 instance.
# Set OMV_HOST / OMV_USERNAME / OMV_PASSWORD (and optionally OMV_PORT,
# OMV_SCHEME, OMV_INSECURE_SKIP_VERIFY) before running.
testacc:
	TF_ACC=1 go test -count=1 -v -timeout 30m ./...

# Regenerates docs/ from the schema descriptions + examples/ using
# tfplugindocs (https://github.com/hashicorp/terraform-plugin-docs).
generate:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@latest generate

clean:
	rm -rf bin dist
