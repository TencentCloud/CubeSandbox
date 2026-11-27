#define _GNU_SOURCE

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <sched.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/file.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#ifndef SYS_open_tree
#define SYS_open_tree 428
#endif
#ifndef SYS_move_mount
#define SYS_move_mount 429
#endif
#ifndef OPEN_TREE_CLONE
#define OPEN_TREE_CLONE 1
#endif
#ifndef OPEN_TREE_CLOEXEC
#define OPEN_TREE_CLOEXEC O_CLOEXEC
#endif
#ifndef AT_RECURSIVE
#define AT_RECURSIVE 0x8000
#endif
#ifndef MOVE_MOUNT_F_EMPTY_PATH
#define MOVE_MOUNT_F_EMPTY_PATH 0x00000004
#endif
#ifndef NSFS_MAGIC
#define NSFS_MAGIC 0x6e736673
#endif

#define DEFAULT_HOST_MNTNS "/proc/1/ns/mnt"
#define DEFAULT_PIN_DIR    "/run/cube-node-mntns"
#define DEFAULT_PIN_FILE   "/run/cube-node-mntns/anchor"
#define MAX_PATHS          64

static void die(const char *fmt, ...) {
    va_list ap;
    fprintf(stderr, "cube-mntns-init: ");
    va_start(ap, fmt);
    vfprintf(stderr, fmt, ap);
    va_end(ap);
    fputc('\n', stderr);
    _exit(1);
}

static bool parse_bool_env(const char *name) {
    const char *v = getenv(name);
    if (!v || !*v) return false;
    if (strcmp(v, "1") == 0 || strcasecmp(v, "t") == 0 || strcasecmp(v, "true") == 0)
        return true;
    if (strcmp(v, "0") == 0 || strcasecmp(v, "f") == 0 || strcasecmp(v, "false") == 0)
        return false;
    die("invalid boolean %s='%s'", name, v);
    return false;
}

static int mkdir_p(const char *path, mode_t mode) {
    char buf[PATH_MAX];
    size_t n = strlen(path);
    if (n == 0 || n >= sizeof(buf)) {
        errno = ENAMETOOLONG;
        return -1;
    }
    memcpy(buf, path, n + 1);
    for (char *p = buf + 1; *p; p++) {
        if (*p != '/') continue;
        *p = '\0';
        if (mkdir(buf, mode) != 0 && errno != EEXIST) return -1;
        *p = '/';
    }
    if (mkdir(buf, mode) != 0 && errno != EEXIST) return -1;
    return 0;
}

static bool is_nsfs_file(const char *path) {
    struct statfs sfs;
    return statfs(path, &sfs) == 0 && (unsigned long)sfs.f_type == NSFS_MAGIC;
}

static bool is_mountpoint(const char *target) {
    FILE *fp = fopen("/proc/self/mountinfo", "r");
    if (!fp) return false;
    char line[8192];
    char mp[PATH_MAX];
    bool found = false;
    while (fgets(line, sizeof(line), fp)) {
        if (sscanf(line, "%*d %*d %*u:%*u %*s %4095s", mp) == 1 &&
            strcmp(mp, target) == 0) {
            found = true;
            break;
        }
    }
    fclose(fp);
    return found;
}

static int split_paths(const char *raw, char *out[], int max_out) {
    if (!raw || !*raw) return 0;
    char *copy = strdup(raw);
    if (!copy) die("strdup: %s", strerror(errno));

    int count = 0;
    char *save = NULL;
    for (char *tok = strtok_r(copy, ":", &save); tok; tok = strtok_r(NULL, ":", &save)) {
        size_t len = strlen(tok);
        while (len > 1 && tok[len - 1] == '/') tok[--len] = '\0';
        if (len == 0) continue;
        if (tok[0] != '/' || strcmp(tok, "/") == 0)
            die("invalid shared mount path '%s' (must be absolute and non-root)", tok);
        if (strstr(tok, "/../") != NULL ||
            (len >= 3 && strcmp(tok + len - 3, "/..") == 0))
            die("invalid shared mount path '%s' (contains '..')", tok);

        bool dup = false;
        for (int i = 0; i < count; i++) {
            if (strcmp(out[i], tok) == 0) {
                dup = true;
                break;
            }
        }
        if (dup) continue;
        if (count >= max_out) die("too many shared mount paths (max %d)", max_out);
        out[count] = strdup(tok);
        if (!out[count]) die("strdup: %s", strerror(errno));
        count++;
    }
    free(copy);
    return count;
}

static void join_path(char *dst, size_t sz, const char *a, const char *b) {
    int n = snprintf(dst, sz, "%s%s", a, b);
    if (n < 0 || (size_t)n >= sz) die("path too long: %s%s", a, b);
}

static void sync_existing_anchor(const char *pin_file, char *paths[], int npaths) {
    int anchor_fd = open(pin_file, O_RDONLY | O_CLOEXEC);
    if (anchor_fd < 0) die("open anchor %s: %s", pin_file, strerror(errno));

    pid_t pid = fork();
    if (pid < 0) die("fork sync_existing_anchor: %s", strerror(errno));
    if (pid == 0) {
        if (unshare(CLONE_NEWNS) != 0) die("unshare sync: %s", strerror(errno));
        if (mount(NULL, "/", NULL, MS_REC | MS_SLAVE, NULL) != 0)
            die("make-rslave sync: %s", strerror(errno));

        int fds[MAX_PATHS];
        for (int i = 0; i < npaths; i++) {
            fds[i] = (int)syscall(SYS_open_tree, AT_FDCWD, paths[i],
                                  OPEN_TREE_CLONE | OPEN_TREE_CLOEXEC | AT_RECURSIVE);
            if (fds[i] < 0) die("open_tree(%s): %s", paths[i], strerror(errno));
        }

        if (setns(anchor_fd, CLONE_NEWNS) != 0)
            die("setns existing anchor: %s", strerror(errno));
        close(anchor_fd);

        struct stat root_st;
        if (stat("/", &root_st) != 0) die("stat anchor /: %s", strerror(errno));

        for (int i = 0; i < npaths; i++) {
            struct stat st;
            if (stat(paths[i], &st) == 0 && st.st_dev != root_st.st_dev) {
                close(fds[i]);
                continue;
            }
            if (mkdir_p(paths[i], 0755) != 0)
                die("mkdir anchor %s: %s", paths[i], strerror(errno));
            if (syscall(SYS_move_mount, fds[i], "", AT_FDCWD, paths[i],
                        MOVE_MOUNT_F_EMPTY_PATH) != 0)
                die("move_mount anchor %s: %s", paths[i], strerror(errno));
            close(fds[i]);
            if (mount(NULL, paths[i], NULL, MS_REC | MS_SHARED, NULL) != 0)
                die("make-rshared anchor %s: %s", paths[i], strerror(errno));
        }
        _exit(0);
    }

    close(anchor_fd);
    int st = 0;
    if (waitpid(pid, &st, 0) < 0) die("waitpid sync_existing_anchor: %s", strerror(errno));
    if (!WIFEXITED(st) || WEXITSTATUS(st) != 0)
        die("sync_existing_anchor failed");
}

/*
 * Build (or sync) the node-level anchor mount namespace A_node inside the host
 * root mntns, and pin it at pin_file.
 *
 * Inside A_node:
 *   /         : minimal 4 MiB private tmpfs
 *   paths[i]  : master:H_i, shared:C_i (H -> C one-way; C <-> C bidirectional;
 *               C -> H completely blocked so host systemd never sees sandbox mounts)
 */
static void build_anchor_in_host_mntns(const char *host_mntns,
                                       const char *pin_dir,
                                       const char *pin_file,
                                       char *paths[], int npaths) {
    int host_fd = open(host_mntns, O_RDONLY | O_CLOEXEC);
    if (host_fd < 0) die("open host mntns %s: %s", host_mntns, strerror(errno));
    if (setns(host_fd, CLONE_NEWNS) != 0)
        die("setns host mntns %s: %s", host_mntns, strerror(errno));
    close(host_fd);

    if (mkdir_p(pin_dir, 0700) != 0)
        die("mkdir %s: %s", pin_dir, strerror(errno));

    char lock_path[PATH_MAX];
    join_path(lock_path, sizeof(lock_path), pin_dir, "/.lock");
    int lock_fd = open(lock_path, O_CREAT | O_RDWR | O_CLOEXEC, 0600);
    if (lock_fd < 0) die("open lock %s: %s", lock_path, strerror(errno));
    if (flock(lock_fd, LOCK_EX) != 0) die("flock %s: %s", lock_path, strerror(errno));

    /* Ensure each shared dataplane path is a shared mountpoint in host mntns. */
    for (int i = 0; i < npaths; i++) {
        if (mkdir_p(paths[i], 0755) != 0)
            die("mkdir host path %s: %s", paths[i], strerror(errno));
        if (!is_mountpoint(paths[i])) {
            if (mount(paths[i], paths[i], NULL, MS_BIND, NULL) != 0)
                die("self-bind host path %s: %s", paths[i], strerror(errno));
        }
        if (mount(NULL, paths[i], NULL, MS_SHARED, NULL) != 0)
            die("make-shared host path %s: %s", paths[i], strerror(errno));
    }

    if (is_nsfs_file(pin_file)) {
        sync_existing_anchor(pin_file, paths, npaths);
        close(lock_fd);
        return;
    }

    /* Make pin_dir a private self-bind so pinning anchor never propagates to host peers. */
    if (!is_mountpoint(pin_dir)) {
        if (mount(pin_dir, pin_dir, NULL, MS_BIND, NULL) != 0)
            die("bind %s: %s", pin_dir, strerror(errno));
    }
    if (mount(NULL, pin_dir, NULL, MS_PRIVATE, NULL) != 0)
        die("make-private %s: %s", pin_dir, strerror(errno));

    int pf = open(pin_file, O_CREAT | O_RDONLY | O_CLOEXEC, 0600);
    if (pf < 0) die("create pin file %s: %s", pin_file, strerror(errno));
    close(pf);

    int ready_pipe[2], done_pipe[2];
    if (pipe2(ready_pipe, O_CLOEXEC) != 0 || pipe2(done_pipe, O_CLOEXEC) != 0)
        die("pipe2: %s", strerror(errno));

    pid_t gc = fork();
    if (gc < 0) die("fork anchor builder: %s", strerror(errno));
    if (gc == 0) {
        close(ready_pipe[0]);
        close(done_pipe[1]);

        if (unshare(CLONE_NEWNS) != 0) die("unshare anchor: %s", strerror(errno));
        if (mount(NULL, "/", NULL, MS_REC | MS_SLAVE, NULL) != 0)
            die("make-rslave /: %s", strerror(errno));

        char stage[PATH_MAX];
        join_path(stage, sizeof(stage), pin_dir, "/.stage");
        if (mkdir_p(stage, 0700) != 0) die("mkdir %s: %s", stage, strerror(errno));
        if (mount("tmpfs", stage, "tmpfs", 0, "size=4m,mode=0755") != 0)
            die("mount anchor tmpfs: %s", strerror(errno));
        if (mount(NULL, stage, NULL, MS_PRIVATE, NULL) != 0)
            die("make-private anchor tmpfs: %s", strerror(errno));

        for (int i = 0; i < npaths; i++) {
            char dst[PATH_MAX];
            join_path(dst, sizeof(dst), stage, paths[i]);
            if (mkdir_p(dst, 0755) != 0) die("mkdir %s: %s", dst, strerror(errno));
            if (mount(paths[i], dst, NULL, MS_BIND | MS_REC, NULL) != 0)
                die("rbind %s -> %s: %s", paths[i], dst, strerror(errno));
            /* Already slave to H; MS_SHARED adds independent peer group C_i (master:H, shared:C_i). */
            if (mount(NULL, dst, NULL, MS_REC | MS_SHARED, NULL) != 0)
                die("make-rshared %s: %s", dst, strerror(errno));
        }

        if (chdir(stage) != 0) die("chdir %s: %s", stage, strerror(errno));
        if (mkdir(".old", 0700) != 0 && errno != EEXIST)
            die("mkdir .old: %s", strerror(errno));
        if (syscall(SYS_pivot_root, ".", ".old") != 0)
            die("pivot_root anchor: %s", strerror(errno));
        if (chdir("/") != 0) die("chdir /: %s", strerror(errno));
        if (umount2("/.old", MNT_DETACH) != 0)
            die("umount /.old: %s", strerror(errno));
        rmdir("/.old");

        char ok = '1';
        if (write(ready_pipe[1], &ok, 1) != 1) _exit(1);
        close(ready_pipe[1]);
        char buf;
        if (read(done_pipe[0], &buf, 1) < 0) {}
        _exit(0);
    }

    close(ready_pipe[1]);
    close(done_pipe[0]);

    char ok = 0;
    ssize_t n = read(ready_pipe[0], &ok, 1);
    close(ready_pipe[0]);
    if (n != 1 || ok != '1') {
        close(done_pipe[1]);
        waitpid(gc, NULL, 0);
        die("anchor builder child failed");
    }

    char gc_ns[64];
    snprintf(gc_ns, sizeof(gc_ns), "/proc/%d/ns/mnt", gc);
    if (mount(gc_ns, pin_file, NULL, MS_BIND, NULL) != 0) {
        int saved = errno;
        close(done_pipe[1]);
        waitpid(gc, NULL, 0);
        die("bind anchor %s -> %s: %s", gc_ns, pin_file, strerror(saved));
    }

    close(done_pipe[1]);
    int st = 0;
    waitpid(gc, &st, 0);
    if (!WIFEXITED(st) || WEXITSTATUS(st) != 0)
        die("anchor builder exited abnormally");
    close(lock_fd);
}

static void ensure_anchor(const char *host_mntns, const char *pin_dir,
                          const char *pin_file, char *paths[], int npaths) {
    pid_t pid = fork();
    if (pid < 0) die("fork ensure_anchor: %s", strerror(errno));
    if (pid == 0) {
        build_anchor_in_host_mntns(host_mntns, pin_dir, pin_file, paths, npaths);
        _exit(0);
    }
    int st = 0;
    if (waitpid(pid, &st, 0) < 0) die("waitpid ensure_anchor: %s", strerror(errno));
    if (!WIFEXITED(st) || WEXITSTATUS(st) != 0)
        die("ensure_anchor failed (status=%d)", WIFEXITED(st) ? WEXITSTATUS(st) : -1);
}

/*
 * Assemble this container's private mount namespace M_cubelet:
 *   1. Clone current container rootfs tree with open_tree(OPEN_TREE_CLONE | AT_RECURSIVE).
 *   2. Step into host mntns to open pin_file, enter A_node, and unshare(CLONE_NEWNS)
 *      so we inherit shared:C_i peer groups.
 *   3. Attach container rootfs at /.cube_newroot (made private to accept MS_MOVE).
 *   4. MS_MOVE each shared dataplane mount from A_node into /.cube_newroot<path>.
 *   5. pivot_root into /.cube_newroot and detach the temporary anchor root.
 */
static void attach_to_anchor(const char *host_mntns, const char *pin_file,
                             char *paths[], int npaths) {
    int orig_cwd = open(".", O_RDONLY | O_DIRECTORY | O_CLOEXEC);

    int root_fd = (int)syscall(SYS_open_tree, AT_FDCWD, "/",
                               OPEN_TREE_CLONE | OPEN_TREE_CLOEXEC | AT_RECURSIVE);
    if (root_fd < 0) die("open_tree(/): %s", strerror(errno));

    int host_fd = open(host_mntns, O_RDONLY | O_CLOEXEC);
    if (host_fd < 0) die("open host mntns %s: %s", host_mntns, strerror(errno));
    if (setns(host_fd, CLONE_NEWNS) != 0)
        die("setns host mntns %s: %s", host_mntns, strerror(errno));
    close(host_fd);

    int ns_fd = open(pin_file, O_RDONLY | O_CLOEXEC);
    if (ns_fd < 0) die("open anchor %s: %s", pin_file, strerror(errno));
    if (setns(ns_fd, CLONE_NEWNS) != 0) die("setns anchor: %s", strerror(errno));
    close(ns_fd);

    if (unshare(CLONE_NEWNS) != 0) die("unshare container mntns: %s", strerror(errno));
    /* Keep anchor root tmpfs private (non-recursive) so MS_MOVE out of / succeeds. */
    if (mount(NULL, "/", NULL, MS_PRIVATE, NULL) != 0)
        die("make-private anchor /: %s", strerror(errno));

    if (mkdir("/.cube_newroot", 0755) != 0 && errno != EEXIST)
        die("mkdir /.cube_newroot: %s", strerror(errno));
    if (syscall(SYS_move_mount, root_fd, "", AT_FDCWD, "/.cube_newroot",
                MOVE_MOUNT_F_EMPTY_PATH) != 0)
        die("move_mount newroot: %s", strerror(errno));
    close(root_fd);

    /* Destination parent must not be shared for MS_MOVE, and detach must stay local. */
    if (mount(NULL, "/.cube_newroot", NULL, MS_REC | MS_PRIVATE, NULL) != 0)
        die("make-rprivate /.cube_newroot: %s", strerror(errno));

    for (int i = 0; i < npaths; i++) {
        char dst[PATH_MAX];
        join_path(dst, sizeof(dst), "/.cube_newroot", paths[i]);
        if (mkdir_p(dst, 0755) != 0) die("mkdir %s: %s", dst, strerror(errno));
        /* Detach any shadowed container-side hostPath mount before moving the shared peer in. */
        (void)umount2(dst, MNT_DETACH);
        if (mount(paths[i], dst, NULL, MS_MOVE, NULL) != 0)
            die("MS_MOVE %s -> %s: %s", paths[i], dst, strerror(errno));
    }

    if (chdir("/.cube_newroot") != 0) die("chdir /.cube_newroot: %s", strerror(errno));
    if (mkdir(".cube_oldroot", 0700) != 0 && errno != EEXIST)
        die("mkdir .cube_oldroot: %s", strerror(errno));
    if (syscall(SYS_pivot_root, ".", ".cube_oldroot") != 0)
        die("pivot_root newroot: %s", strerror(errno));
    if (chdir("/") != 0) die("chdir /: %s", strerror(errno));
    if (umount2("/.cube_oldroot", MNT_DETACH) != 0)
        die("umount /.cube_oldroot: %s", strerror(errno));
    rmdir("/.cube_oldroot");

    if (orig_cwd >= 0) {
        if (fchdir(orig_cwd) != 0) {}
        close(orig_cwd);
    }
}

int main(int argc, char **argv) {
    const char *host_mntns = DEFAULT_HOST_MNTNS;
    const char *pin_dir    = DEFAULT_PIN_DIR;
    const char *pin_file   = DEFAULT_PIN_FILE;
    const char *paths_raw  = getenv("CUBELET_SHARED_MNT_PATHS");

    int i = 1;
    for (; i < argc; i++) {
        if (strcmp(argv[i], "--") == 0) {
            i++;
            break;
        } else if (strcmp(argv[i], "--host-mntns") == 0 && i + 1 < argc) {
            host_mntns = argv[++i];
        } else if (strcmp(argv[i], "--pin-dir") == 0 && i + 1 < argc) {
            pin_dir = argv[++i];
        } else if (strcmp(argv[i], "--pin-file") == 0 && i + 1 < argc) {
            pin_file = argv[++i];
        } else if (strcmp(argv[i], "--paths") == 0 && i + 1 < argc) {
            paths_raw = argv[++i];
        } else if (argv[i][0] == '-') {
            die("unknown flag: %s", argv[i]);
        } else {
            break;
        }
    }

    if (i >= argc)
        die("usage: %s [--host-mntns PATH] [--pin-dir PATH] [--pin-file PATH] [--paths P1:P2] -- CMD [ARGS...]",
            argv[0]);

    char **cmd_argv = &argv[i];

    if (parse_bool_env("CUBELET_DEPLOYED_IN_BIG_POD")) {
        char *paths[MAX_PATHS];
        int npaths = split_paths(paths_raw, paths, MAX_PATHS);
        if (npaths == 0)
            die("CUBELET_DEPLOYED_IN_BIG_POD is true but CUBELET_SHARED_MNT_PATHS is empty");

        ensure_anchor(host_mntns, pin_dir, pin_file, paths, npaths);
        attach_to_anchor(host_mntns, pin_file, paths, npaths);

        for (int k = 0; k < npaths; k++) free(paths[k]);
    }

    execvp(cmd_argv[0], cmd_argv);
    die("execvp(%s): %s", cmd_argv[0], strerror(errno));
    return 127;
}
