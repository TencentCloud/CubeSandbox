// SPDX-License-Identifier: GPL-2.0
/*
 * cube_gauge — kthread GAUGE collector (layout=2, whole-only).
 *
 * One sandbox == one container: collect machine-wide CPU/mem/disk/net,
 * write a single "whole" record. No per-cgroup slots, no idmap.
 *
 * This module only transports kernel counters; it computes no rates. Every
 * field but mem_bytes is a raw cumulative value that the reader
 * differentiates, so nothing here has to track previous samples.
 *
 * cgroup v1 and v2: detect the hierarchy first (statfs magic), then
 * resolve files on that path only.
 *   v2 (cgroup2): CPU = cpu.stat (usage_usec, us -> ns)
 *                 mem = memory.current (root, else default/)
 *   v1 (otherwise): CPU = cpuacct/cpuacct.usage or cpu,cpuacct/cpuacct.usage
 *                   mem = memory/memory.usage_in_bytes
 *
 * System disk and NIC come from the kernel structures behind
 * /proc/diskstats and /proc/net/dev rather than from those files:
 *   disk = block_device->bd_stats via part_stat_read()
 *   net  = dev_get_stats() over for_each_netdev_rcu()
 *
 * Reads cgroup files only; does not use memcg / bstat / bpf / kallsyms.
 *
 * Once insmod succeeds the module pins itself (try_module_get), so
 * rmmod always returns -EBUSY. Collection stops only when the sandbox
 * VM is destroyed.
 */
#include <linux/module.h>
#include <linux/kernel.h>
#include <linux/kthread.h>
#include <linux/delay.h>
#include <linux/fs.h>
#include <linux/file.h>
#include <linux/namei.h>
#include <linux/statfs.h>
#include <linux/magic.h>
#include <linux/pci.h>
#include <linux/io.h>
#include <linux/ktime.h>
#include <linux/slab.h>
#include <linux/uaccess.h>
#include <linux/version.h>
#include <linux/blkdev.h>
#include <linux/blk_types.h>
#include <linux/part_stat.h>
#include <linux/netdevice.h>
#include <linux/rcupdate.h>
#include <net/net_namespace.h>

#include "gauge.h"

#define DRV_NAME "cube_gauge"
#define CUBE_GAUGE_SUBSYS 0x0101
#define MAX_PATH 512
#define READ_BUF 256

/*
 * Sampling interval bounds. These are a security guard, not a preference:
 * the parameter is writable from inside the guest, so a guest that gains
 * root must not be able to pin the collector at a few milliseconds and burn
 * CPU, nor stretch the interval far enough to hide its activity between
 * samples.
 */
#define INTERVAL_MIN_MS 1000u
#define INTERVAL_MAX_MS 300000u

static unsigned int interval_ms = 15000;

/*
 * Out-of-range writes are rejected rather than clamped so that reading the
 * parameter back always reports the interval actually in effect. Applies to
 * the insmod argument as well, which makes a bad value fail the load loudly
 * instead of silently running at a different rate.
 */
static int interval_ms_set(const char *val, const struct kernel_param *kp)
{
	unsigned int v;
	int ret;

	ret = kstrtouint(val, 0, &v);
	if (ret)
		return ret;
	if (v < INTERVAL_MIN_MS || v > INTERVAL_MAX_MS) {
		pr_warn(DRV_NAME ": interval_ms=%u rejected (allowed %u-%u)\n",
			v, INTERVAL_MIN_MS, INTERVAL_MAX_MS);
		return -EINVAL;
	}
	*(unsigned int *)kp->arg = v;
	return 0;
}

static const struct kernel_param_ops interval_ms_ops = {
	.set = interval_ms_set,
	.get = param_get_uint,
};
module_param_cb(interval_ms, &interval_ms_ops, &interval_ms, 0644);
MODULE_PARM_DESC(interval_ms, "sample interval in ms (1000-300000)");

/*
 * Both are consumed once during init -- cgroup_root by detect_cgroup() and
 * shmem_path by file_fallback_setup() -- so a later write would change
 * nothing except what a reader believes is in use. Keep them read-only to
 * avoid that discrepancy.
 */
static char cgroup_root[256] = "/sys/fs/cgroup";
module_param_string(cgroup_root, cgroup_root, sizeof(cgroup_root), 0444);

static char shmem_path[256] = "/dev/shm/cube-gauge.shmem";
module_param_string(shmem_path, shmem_path, sizeof(shmem_path), 0444);

static unsigned long shmem_size = 4096;
module_param(shmem_size, ulong, 0444);

static int autostart = 1;
module_param(autostart, int, 0444);

/*
 * System disk. Read-only after init because the resolved block_device is
 * cached for the lifetime of the module.
 *
 * Defaults to the container writable layer (vda). Note that pmem0/pmem1 are
 * DAX-mapped, so they bypass the block layer entirely and their bd_stats are
 * permanently zero -- pointing this at a pmem device yields no traffic.
 */
static char sysdisk[128] = "/dev/vda";
module_param_string(sysdisk, sysdisk, sizeof(sysdisk), 0444);
MODULE_PARM_DESC(sysdisk, "system disk block device path (default /dev/vda)");

/*
 * NIC to account. Empty = first non-loopback interface that is up.
 * Read-only after init: collect_net() consults this every tick, so a
 * writable copy would let the guest silence NIC accounting by pointing it
 * at an interface that does not exist.
 */
static char netif[IFNAMSIZ];
module_param_string(netif, netif, sizeof(netif), 0444);
MODULE_PARM_DESC(netif, "interface name, empty = first non-loopback up link");

/* cgroup mode + resolved accounting file paths (decided at init) */
static int cg_mode;			/* CUBE_GAUGE_CG_V2 / _V1 */
static char cpu_path[MAX_PATH];		/* absolute CPU usage file */
static char mem_path[MAX_PATH];		/* absolute mem usage file */

struct cube_gauge {
	void __iomem *bar;
	struct file *file;
	size_t size;
	u64 global_seq;
	struct task_struct *task;
	struct pci_dev *pdev;
	struct block_device *sysdisk_bdev;
};

static struct cube_gauge g_st;
static DEFINE_MUTEX(g_lock);
static bool have_pci;
static bool self_pinned;

static unsigned int clamp_interval(void)
{
	unsigned int v = READ_ONCE(interval_ms);

	/* Belt and braces: interval_ms_set() already refuses these. */
	if (v < INTERVAL_MIN_MS)
		v = INTERVAL_MIN_MS;
	if (v > INTERVAL_MAX_MS)
		v = INTERVAL_MAX_MS;
	return v;
}

static int gauge_write(size_t off, const void *src, size_t n)
{
	if (off + n > g_st.size)
		return -EINVAL;
	if (g_st.bar) {
		memcpy_toio(g_st.bar + off, src, n);
		return 0;
	}
	if (g_st.file) {
		loff_t pos = (loff_t)off;
		ssize_t w = kernel_write(g_st.file, src, n, &pos);

		return (w == (ssize_t)n) ? 0 : -EIO;
	}
	return -ENODEV;
}

static int gauge_read(size_t off, void *dst, size_t n)
{
	if (off + n > g_st.size)
		return -EINVAL;
	if (g_st.bar) {
		memcpy_fromio(dst, g_st.bar + off, n);
		return 0;
	}
	if (g_st.file) {
		loff_t pos = (loff_t)off;
		ssize_t r = kernel_read(g_st.file, dst, n, &pos);

		return (r == (ssize_t)n) ? 0 : -EIO;
	}
	return -ENODEV;
}

static bool header_ok(void)
{
	struct cube_gauge_header hdr;

	if (gauge_read(0, &hdr, sizeof(hdr)))
		return false;
	return hdr.magic == CUBE_GAUGE_MAGIC &&
	       hdr.layout == CUBE_GAUGE_LAYOUT &&
	       hdr.total_size == g_st.size;
}

static void write_header(void)
{
	struct cube_gauge_header hdr;

	memset(&hdr, 0, sizeof(hdr));
	hdr.magic = CUBE_GAUGE_MAGIC;
	hdr.version = CUBE_GAUGE_VERSION;
	hdr.layout = CUBE_GAUGE_LAYOUT;
	hdr.header_size = CUBE_GAUGE_HEADER_SIZE;
	hdr.whole_size = CUBE_GAUGE_WHOLE_SIZE;
	hdr.total_size = g_st.size;
	hdr.cgroup_mode = cg_mode;
	gauge_write(0, &hdr, sizeof(hdr));
}

static void clear_whole(void)
{
	struct cube_gauge_global z;

	memset(&z, 0, sizeof(z));
	gauge_write(CUBE_GAUGE_WHOLE_OFFSET, &z, sizeof(z));
	g_st.global_seq = 0;
}

static int setup_backing(size_t size)
{
	if (size < CUBE_GAUGE_MIN_SIZE)
		return -ENOMEM;
	g_st.size = size;
	write_header();
	clear_whole();
	pr_info(DRV_NAME ": ready size=%zu mode=%s\n", size,
		cg_mode == CUBE_GAUGE_CG_V1 ? "v1" : "v2");
	return 0;
}

static int read_file_str(const char *path, char *buf, size_t buflen)
{
	struct file *f;
	loff_t pos = 0;
	ssize_t n;

	f = filp_open(path, O_RDONLY, 0);
	if (IS_ERR(f))
		return -ENOENT;
	n = kernel_read(f, buf, buflen - 1, &pos);
	filp_close(f, NULL);
	if (n < 0)
		return (int)n;
	buf[n] = '\0';
	return 0;
}

static bool file_readable(const char *path)
{
	struct file *f = filp_open(path, O_RDONLY, 0);

	if (IS_ERR(f))
		return false;
	filp_close(f, NULL);
	return true;
}

static bool path_join(char *dst, size_t n, const char *root, const char *rel)
{
	int w = snprintf(dst, n, "%s/%s", root, rel);

	return w > 0 && w < (int)n;
}

/*
 * After the hierarchy is classified, pick the first readable relative
 * path. v2 cpu.stat and memory.current are probed separately so a root
 * that has cpu.stat but not memory.current can still use default/.
 */
static bool pick_readable(char *dst, size_t n, const char *root,
			  const char * const *rels)
{
	char cand[MAX_PATH];
	int i;

	for (i = 0; rels[i]; i++) {
		if (!path_join(cand, sizeof(cand), root, rels[i]))
			continue;
		if (!file_readable(cand))
			continue;
		memcpy(dst, cand, sizeof(cand));
		return true;
	}
	return false;
}

static int detect_cgroup_v2(void)
{
	static const char * const cpu_rel[] = {
		"cpu.stat",
		"default/cpu.stat",
		NULL,
	};
	static const char * const mem_rel[] = {
		"memory.current",
		"default/memory.current",
		NULL,
	};

	cg_mode = CUBE_GAUGE_CG_V2;
	if (!pick_readable(cpu_path, sizeof(cpu_path), cgroup_root, cpu_rel) ||
	    !pick_readable(mem_path, sizeof(mem_path), cgroup_root, mem_rel)) {
		pr_err(DRV_NAME ": cgroup v2 but missing cpu.stat/memory.current under %s\n",
		       cgroup_root);
		return -ENOENT;
	}
	return 0;
}

static int detect_cgroup_v1(void)
{
	static const char * const cpu_rel[] = {
		"cpuacct/cpuacct.usage",
		"cpu,cpuacct/cpuacct.usage",
		NULL,
	};
	static const char * const mem_rel[] = {
		"memory/memory.usage_in_bytes",
		NULL,
	};

	cg_mode = CUBE_GAUGE_CG_V1;
	if (!pick_readable(cpu_path, sizeof(cpu_path), cgroup_root, cpu_rel) ||
	    !pick_readable(mem_path, sizeof(mem_path), cgroup_root, mem_rel)) {
		pr_err(DRV_NAME ": cgroup v1 but missing cpuacct/memory files under %s\n",
		       cgroup_root);
		return -ENOENT;
	}
	return 0;
}

/*
 * 1) Classify: cgroup2 superblock → v2, otherwise v1 (tmpfs of controllers
 *    or a cgroup v1 mount).
 * 2) Resolve only that mode's files. Parsing later follows cg_mode.
 */
static int detect_cgroup(void)
{
	struct path path;
	struct kstatfs st;
	int ret;

	ret = kern_path(cgroup_root, LOOKUP_FOLLOW, &path);
	if (ret) {
		pr_err(DRV_NAME ": cannot open %s: %d\n", cgroup_root, ret);
		return -EOPNOTSUPP;
	}
	ret = vfs_statfs(&path, &st);
	path_put(&path);
	if (ret) {
		pr_err(DRV_NAME ": statfs %s failed: %d\n", cgroup_root, ret);
		return -EOPNOTSUPP;
	}

	if (st.f_type == CGROUP2_SUPER_MAGIC)
		ret = detect_cgroup_v2();
	else
		ret = detect_cgroup_v1();
	if (ret)
		return ret;

	pr_info(DRV_NAME ": mode=%s cpu=%s mem=%s\n",
		cg_mode == CUBE_GAUGE_CG_V1 ? "v1" : "v2", cpu_path, mem_path);
	return 0;
}

/* CPU cumulative usage in ns (v2: cpu.stat usage_usec; v1: cpuacct.usage ns) */
static int parse_usage_ns(u64 *out)
{
	char buf[READ_BUF];
	char *p, *nl;
	u64 val;

	if (read_file_str(cpu_path, buf, sizeof(buf)))
		return -ENOENT;

	if (cg_mode == CUBE_GAUGE_CG_V2) {
		p = strstr(buf, "usage_usec");
		if (!p)
			return -EINVAL;
		p += strlen("usage_usec");
		while (*p == ' ' || *p == '\t')
			p++;
		nl = strchr(p, '\n');
		if (nl)
			*nl = '\0';
		if (kstrtou64(p, 10, &val))
			return -EINVAL;
		*out = val * 1000ULL;	/* us -> ns */
		return 0;
	}

	/* v1: cpuacct.usage is a bare number in ns */
	nl = strchr(buf, '\n');
	if (nl)
		*nl = '\0';
	if (kstrtou64(buf, 10, &val))
		return -EINVAL;
	*out = val;
	return 0;
}

static int parse_mem_bytes(u64 *out)
{
	char buf[64];
	char *nl;

	if (read_file_str(mem_path, buf, sizeof(buf)))
		return -ENOENT;
	nl = strchr(buf, '\n');
	if (nl)
		*nl = '\0';
	return kstrtou64(buf, 10, out) ? -EINVAL : 0;
}

/*
 * System disk counters, straight out of the kernel structure that
 * /proc/diskstats renders: block_device->bd_stats, a per-cpu disk_stats.
 * part_stat_read() (include/linux/part_stat.h) is the exported macro form of
 * the per-cpu sum; block/genhd.c does the same thing in its private
 * part_stat_read_all(), which is static and therefore off limits here.
 * Sectors are the kernel's fixed 512-byte units regardless of logical
 * block size, so << 9 gives bytes.
 */
static bool collect_sysdisk(struct cube_gauge_global *g)
{
	struct block_device *bdev = g_st.sysdisk_bdev;

	if (!bdev) {
		/*
		 * Resolved lazily: the module can be loaded before udev has
		 * populated /dev, so a failure here is retried next tick.
		 */
		bdev = blkdev_get_by_path(sysdisk, BLK_OPEN_READ, NULL, NULL);
		if (IS_ERR(bdev))
			return false;
		g_st.sysdisk_bdev = bdev;
		pr_info(DRV_NAME ": sysdisk %s\n", sysdisk);
	}

	g->sysdisk_read_bytes = (u64)part_stat_read(bdev, sectors[STAT_READ]) << 9;
	g->sysdisk_write_bytes = (u64)part_stat_read(bdev, sectors[STAT_WRITE]) << 9;
	g->sysdisk_read_ios = (u64)part_stat_read(bdev, ios[STAT_READ]);
	g->sysdisk_write_ios = (u64)part_stat_read(bdev, ios[STAT_WRITE]);
	return true;
}

static void sysdisk_release(void)
{
	if (g_st.sysdisk_bdev) {
		blkdev_put(g_st.sysdisk_bdev, NULL);
		g_st.sysdisk_bdev = NULL;
	}
}

/*
 * NIC counters via dev_get_stats(), which is exactly what /proc/net/dev is
 * rendered from (net/core/net-procfs.c). RCU is the lock procfs uses for
 * this walk too; RTNL would sleep and is not needed for a read.
 */
static bool collect_net(struct cube_gauge_global *g)
{
	const struct rtnl_link_stats64 *st;
	struct rtnl_link_stats64 tmp;
	struct net_device *dev;
	bool got = false;

	rcu_read_lock();
	for_each_netdev_rcu(&init_net, dev) {
		if (dev->flags & IFF_LOOPBACK)
			continue;
		if (netif[0]) {
			if (strcmp(dev->name, netif))
				continue;
		} else if (!(dev->flags & IFF_UP)) {
			continue;
		}

		st = dev_get_stats(dev, &tmp);
		g->net_rx_bytes = st->rx_bytes;
		g->net_tx_bytes = st->tx_bytes;
		g->net_rx_packets = st->rx_packets;
		g->net_tx_packets = st->tx_packets;
		got = true;
		break;		/* one sandbox == one NIC */
	}
	rcu_read_unlock();

	return got;
}

/*
 * Seqlock publish: odd sequence marks the record in flux, even marks it
 * settled. The first store also blanks the payload so a reader that catches
 * the odd phase cannot mix new and stale fields.
 */
static void write_global(const struct cube_gauge_global *src)
{
	struct cube_gauge_global g;
	u64 seq;

	seq = g_st.global_seq + 1;
	if (!(seq & 1ULL))
		seq++;

	memset(&g, 0, sizeof(g));
	g.sequence = seq;
	gauge_write(CUBE_GAUGE_WHOLE_OFFSET, &g, sizeof(g));

	g = *src;
	g.sequence = seq + 1;
	g_st.global_seq = seq + 1;
	gauge_write(CUBE_GAUGE_WHOLE_OFFSET, &g, sizeof(g));
}

static void collect_once(void)
{
	struct cube_gauge_global g;
	u64 now, usage, mem;

	now = ktime_get_boottime_ns();
	if (!header_ok()) {
		write_header();
		clear_whole();
	}

	memset(&g, 0, sizeof(g));
	g.timestamp_ns = now;

	if (parse_usage_ns(&usage))
		usage = 0;
	if (parse_mem_bytes(&mem))
		mem = 0;
	/* cgroup may be unreadable while disk/NIC still are, so keep going. */
	if (usage || mem || file_readable(cpu_path)) {
		g.flags |= CUBE_GAUGE_FLAG_VALID;
		g.cpu_usage_ns = usage;
		g.mem_bytes = mem;
	}

	if (collect_sysdisk(&g))
		g.flags |= CUBE_GAUGE_FLAG_SYSDISK_VALID;
	if (collect_net(&g))
		g.flags |= CUBE_GAUGE_FLAG_NET_VALID;

	write_global(&g);
}

static int gauge_thread(void *data)
{
	(void)data;
	while (!kthread_should_stop()) {
		mutex_lock(&g_lock);
		if (g_st.bar || g_st.file)
			collect_once();
		mutex_unlock(&g_lock);
		schedule_timeout_interruptible(
			msecs_to_jiffies(clamp_interval()));
	}
	return 0;
}

static int start_kthread(void)
{
	if (g_st.task)
		return 0;
	g_st.task = kthread_run(gauge_thread, NULL, DRV_NAME);
	if (IS_ERR(g_st.task)) {
		int err = PTR_ERR(g_st.task);

		g_st.task = NULL;
		return err;
	}
	return 0;
}

static void stop_kthread(void)
{
	if (g_st.task) {
		kthread_stop(g_st.task);
		g_st.task = NULL;
	}
}

static void reset_header_gone(void)
{
	struct cube_gauge_header hdr;

	memset(&hdr, 0, sizeof(hdr));
	gauge_write(0, &hdr, sizeof(hdr));
}

static int file_fallback_setup(void)
{
	struct file *f;
	int ret;

	if (!shmem_size || (shmem_size & (shmem_size - 1)))
		return -EINVAL;
	f = filp_open(shmem_path, O_RDWR | O_CREAT, 0600);
	if (IS_ERR(f))
		return PTR_ERR(f);
	ret = vfs_truncate(&f->f_path, shmem_size);
	if (ret) {
		filp_close(f, NULL);
		return ret;
	}
	g_st.file = f;
	ret = setup_backing(shmem_size);
	if (ret) {
		filp_close(f, NULL);
		g_st.file = NULL;
		return ret;
	}
	pr_info(DRV_NAME ": no PCI 0x0101, using file fallback\n");
	return 0;
}

static int cube_gauge_probe(struct pci_dev *pdev, const struct pci_device_id *id)
{
	resource_size_t len;
	int ret;

	(void)id;
	if (pdev->subsystem_device != CUBE_GAUGE_SUBSYS)
		return -ENODEV;
	mutex_lock(&g_lock);
	if (g_st.pdev) {
		mutex_unlock(&g_lock);
		return -EBUSY;
	}
	ret = pci_enable_device(pdev);
	if (ret)
		goto out;
	ret = pci_request_regions_exclusive(pdev, DRV_NAME);
	if (ret)
		goto disable;
	g_st.bar = pci_iomap(pdev, 2, 0);
	if (!g_st.bar) {
		ret = -ENOMEM;
		goto release;
	}
	len = pci_resource_len(pdev, 2);
	ret = setup_backing((size_t)len);
	if (ret)
		goto unmap;
	g_st.pdev = pdev;
	have_pci = true;
	pci_set_drvdata(pdev, &g_st);
	if (autostart) {
		ret = start_kthread();
		if (ret)
			goto unmap;
	}
	mutex_unlock(&g_lock);
	pr_info(DRV_NAME ": probe 0x0101 bar2=%pa size=%pa\n",
		&pci_resource_start(pdev, 2), &len);
	return 0;
unmap:
	pci_iounmap(pdev, g_st.bar);
	g_st.bar = NULL;
release:
	pci_release_regions(pdev);
disable:
	pci_disable_device(pdev);
out:
	mutex_unlock(&g_lock);
	return ret;
}

static void cube_gauge_remove(struct pci_dev *pdev)
{
	mutex_lock(&g_lock);
	stop_kthread();
	reset_header_gone();
	sysdisk_release();
	if (g_st.bar) {
		pci_iounmap(pdev, g_st.bar);
		g_st.bar = NULL;
	}
	g_st.pdev = NULL;
	have_pci = false;
	pci_release_regions(pdev);
	pci_disable_device(pdev);
	mutex_unlock(&g_lock);
}

static const struct pci_device_id cube_gauge_ids[] = {
	{ PCI_DEVICE_SUB(0x1af4, 0x1110, PCI_ANY_ID, PCI_ANY_ID) },
	{ 0 }
};
MODULE_DEVICE_TABLE(pci, cube_gauge_ids);

static struct pci_driver cube_gauge_driver = {
	.name = DRV_NAME,
	.id_table = cube_gauge_ids,
	.probe = cube_gauge_probe,
	.remove = cube_gauge_remove,
};

static int __init cube_gauge_init(void)
{
	int ret;

	ret = detect_cgroup();
	if (ret)
		return ret;

	ret = pci_register_driver(&cube_gauge_driver);
	if (ret)
		return ret;
	if (!have_pci) {
		ret = file_fallback_setup();
		if (ret) {
			pr_err(DRV_NAME ": file fallback failed: %d\n", ret);
			pci_unregister_driver(&cube_gauge_driver);
			return ret;
		}
		if (autostart) {
			ret = start_kthread();
			if (ret) {
				reset_header_gone();
				if (g_st.file) {
					filp_close(g_st.file, NULL);
					g_st.file = NULL;
				}
				pci_unregister_driver(&cube_gauge_driver);
				return ret;
			}
		}
	}

	/*
	 * Pin the module: once loaded it must not be unloaded by guest root.
	 * try_module_get bumps the refcount and we never module_put(), so
	 * rmmod always returns -EBUSY. Only VM destroy (whole kernel down)
	 * stops collection. Reached only on the fully-successful path, so a
	 * failed insmod stays cleanly unloadable.
	 */
	if (!try_module_get(THIS_MODULE)) {
		/* Should not happen during init (module is LIVE); unwind. */
		stop_kthread();
		reset_header_gone();
		sysdisk_release();
		if (g_st.file) {
			filp_close(g_st.file, NULL);
			g_st.file = NULL;
		}
		pci_unregister_driver(&cube_gauge_driver);
		return -EIO;
	}
	self_pinned = true;
	pr_info(DRV_NAME ": pinned, rmmod disabled (destroy VM to stop)\n");
	return 0;
}

static void __exit cube_gauge_exit(void)
{
	/*
	 * Dead code in practice: self_pinned keeps refcount > 0 so rmmod
	 * never reaches here. Kept for completeness / forced-unload paths.
	 */
	stop_kthread();
	mutex_lock(&g_lock);
	reset_header_gone();
	sysdisk_release();
	if (g_st.file) {
		filp_close(g_st.file, NULL);
		g_st.file = NULL;
	}
	mutex_unlock(&g_lock);
	pci_unregister_driver(&cube_gauge_driver);
}

module_init(cube_gauge_init);
module_exit(cube_gauge_exit);

MODULE_LICENSE("GPL");
MODULE_AUTHOR("CubeSandbox");
MODULE_DESCRIPTION("cgroup v1/v2 kthread GAUGE collector (whole-only, layout=1) for CubeSandbox 0x0101");
