# Out-of-tree cube_gauge.ko — compile against the guest kernel tree (KDIR).
KDIR ?= /lib/modules/$(shell uname -r)/build
PWD  := $(shell pwd)

obj-m += cube_gauge.o

all:
	@if [ ! -s $(KDIR)/Module.symvers ] && [ -s $(KDIR)/vmlinux.symvers ]; then \
		cp -f $(KDIR)/vmlinux.symvers $(KDIR)/Module.symvers; \
	fi
	@if [ ! -f $(KDIR)/scripts/module.lds ]; then \
		$(MAKE) -C $(KDIR) scripts/module.lds; \
	fi
	$(MAKE) -C $(KDIR) M=$(PWD) KBUILD_MODPOST_WARN=1 modules

clean:
	$(MAKE) -C $(KDIR) M=$(PWD) clean

.PHONY: all clean
