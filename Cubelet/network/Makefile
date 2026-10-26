COVERAGE_PROFILE=.coverage
COVERAGE_REPORT=coverage.html

ifeq ($(V),1)
	Q =
	msg =
	VERBOSE = -v
else
	Q = @
	msg = @printf '  %-8s %s%s\n' "$(1)" "$(notdir $(2))" "$(if $(3), $(3))";
	MAKEFLAGS += --no-print-directory
endif

.PHONY: all
all: lint test

.PHONY: test
test:
	$(call msg,Run tests)
	$(Q)go clean -testcache
	$(Q)go test $(VERBOSE) -cover -coverprofile=$(COVERAGE_PROFILE) ./...

.PHONY: lint
lint:
	$(call msg,Run linters)
	$(Q)go fmt ./...
	$(Q)go vet ./...
	$(Q)golangci-lint run

.PHONY: cover
cover:
	$(call msg,View coverage)
	$(Q)go tool cover -html=$(COVERAGE_PROFILE) -o $(COVERAGE_REPORT)

.PHONY: clean
clean:
	$(call msg,Clean build artifacts)
	$(Q)rm -f $(COVERAGE_PROFILE) $(COVERAGE_REPORT)
