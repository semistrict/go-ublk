.PHONY: build test check fmt

.DEFAULT_GOAL := build

GO ?= go
GO_FILES := $(shell find . -name '*.go' -not -path './.git/*' -not -path './.tmp/*')
UNIT_TEST_RUN := ^(TestIOURing|TestReadFullAt|TestWriteFullAt)

build:
	$(GO) build ./...

test:
	$(GO) test -timeout 20s -run '$(UNIT_TEST_RUN)' ./...

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
