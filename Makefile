.PHONY: build test check coverage fmt

.DEFAULT_GOAL := build

GO ?= go
GO_FILES := $(shell find . -name '*.go' -not -path './.git/*' -not -path './.tmp/*')
# Portable unit tests that run on macOS and in the GitHub-hosted workflow.
UNIT_TEST_RUN := ^(TestIOURing|TestReadFullAt|TestWriteFullAt|TestRequest|TestCtrl|TestQueue|TestZeroCopy|TestServe|TestStop|TestDelete|TestAffinity|TestNewDevice|TestSetParams|TestGetParams|TestDevice|TestReaderAtHandler)
# Report native unit coverage for the core library package instead of diluting it with command packages.
COVERAGE_PACKAGE := .
COVERAGE_PROFILE := .tmp/coverage/unit.cover.out

build:
	$(GO) build ./...

test:
	$(GO) test -timeout 20s -run '$(UNIT_TEST_RUN)' ./...

coverage:
	mkdir -p .tmp/coverage
	$(GO) test -coverprofile=$(COVERAGE_PROFILE) -run '$(UNIT_TEST_RUN)' $(COVERAGE_PACKAGE)
	$(GO) tool cover -func=$(COVERAGE_PROFILE)

check:
	@fmt_out="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$fmt_out" ]; then \
		echo "gofmt needs to be run on:"; \
		echo "$$fmt_out"; \
		exit 1; \
	fi
	$(GO) vet ./...
	golangci-lint run ./...

fmt:
	gofmt -w -s $(GO_FILES)
