GOLANGCI_LINT_VERSION ?= v2.13.1
CUSTOM_GCL_DIR ?= .
CUSTOM_GCL := $(CUSTOM_GCL_DIR)/custom-gcl

.PHONY: lint lint-golangci lint-config lint-config-check custom-gcl nilaway

# lint is the repository lint gate: shared config drift, golangci-lint with the
# kit analyzers, and nilaway.
lint: lint-config-check lint-golangci nilaway

# custom-gcl builds golangci-lint with the plugins in .custom-gcl.yml
# (the kit analyzers from ./lint/gclplugin and nilaway). golangci-lint keeps
# the build cached and skips it when nothing changed.
custom-gcl:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) custom \
		--destination $(CUSTOM_GCL_DIR) --name custom-gcl --version $(GOLANGCI_LINT_VERSION)

lint-golangci: custom-gcl
	$(CUSTOM_GCL) run ./...

nilaway: custom-gcl
	$(CUSTOM_GCL) run --config .golangci.nilaway.yml ./...

# lint-config regenerates .golangci.yml from the canonical config in
# lint/config plus the repository overlay in .golangci.overlay.yml.
lint-config:
	go run ./cmd/kennlint config

lint-config-check:
	go run ./cmd/kennlint config -check
