GOLANGCI_LINT_VERSION ?= v2.13.1
CUSTOM_GCL_DIR ?= .
CUSTOM_GCL := $(CUSTOM_GCL_DIR)/custom-gcl

.PHONY: lint lint-golangci nilaway nilaway-golangci-build
lint: lint-golangci nilaway

lint-golangci:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

nilaway-golangci-build:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) custom \
		--destination $(CUSTOM_GCL_DIR) --name custom-gcl --version $(GOLANGCI_LINT_VERSION)

nilaway: nilaway-golangci-build
	$(CUSTOM_GCL) run --config .golangci.nilaway.yml ./...
