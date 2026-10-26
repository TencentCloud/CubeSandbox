ARCH := $(shell uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/' \
			 | sed 's/ppc64le/powerpc/' | sed 's/mips.*/mips/' \
			 | sed 's/riscv64/riscv/' | sed 's/loongarch.*/loongarch/')
VMLINUX := $(ARCH)/vmlinux.h
BPFTOOL = bpftool

ifeq ($(V),1)
	Q =
	msg =
else
	Q = @
	msg = @printf '  %-8s %s%s\n'					\
		      "$(1)"						\
		      "$(patsubst $(abspath $(OUTPUT))/%,%,$(2))"	\
		      "$(if $(3), $(3))";
	MAKEFLAGS += --no-print-directory
endif

.PHONY: gen
gen: | $(ARCH)
	$(call msg,GEN-VMLINUX)
	$(Q)$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > $(ARCH)/vmlinux.h

$(ARCH):
	$(call msg,MKDIR,$@)
	$(Q)mkdir -p $(ARCH)
