COVERAGE_PROFILE=.coverage
COVERAGE_REPORT=coverage.html

BPF_GENS := $(wildcard *_bpfel.go)
BPF_OBJS := $(wildcard *_bpfel.o)

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
all: deps gen lint test

.PHONY: deps
deps:
	$(call msg,UPDATE DEPS)
	$(Q)go mod tidy

.PHONY: gen
gen:
	$(call msg,GENERATE BPF)
	$(Q)go generate ./...

.PHONY: lint
lint:
	$(call msg,RUN LINTERS)
	$(Q)golangci-lint run --fix

.PHONY: test
test:
	$(call msg,RUN TESTS)
	$(Q)go clean -testcache
	$(Q)go test $(VERBOSE) -cover -coverprofile=$(COVERAGE_PROFILE) ./...
	$(Q)go tool cover -html=$(COVERAGE_PROFILE) -o $(COVERAGE_REPORT)

.PHONY: verify $(BPF_OBJS)
verify: $(BPF_OBJS)

$(BPF_OBJS):
	$(call msg,VERIFY $@)
	$(Q)veristat $@

.PHONY: clean
clean:
	$(call msg,CLEAN)
	$(Q)rm -f $(COVERAGE_PROFILE) $(COVERAGE_REPORT) $(BPF_GENS) $(BPF_OBJS)
