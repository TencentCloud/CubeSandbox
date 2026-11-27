#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/magic.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/vfs.h>
#include <unistd.h>

#ifndef NSFS_MAGIC
#define NSFS_MAGIC 0x6e736673
#endif

#define CUBELET_MNT_NS_PATH "/usr/local/services/cubetoolbox/cubeletmnt/mnt"

int enter_namespace_path(const char *path) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd == -1) {
        if (errno == ENOENT) {
            return 0;
        }
        fprintf(stderr, "%d:%d open mnt namespace %s failed: %s\n", getppid(), getpid(), path, strerror(errno));
        return -1;
    }

    struct statfs st;
    if (fstatfs(fd, &st) == -1) {
        int err = errno;
        close(fd);
        fprintf(stderr, "%d:%d fstatfs mnt namespace %s failed: %s\n", getppid(), getpid(), path, strerror(err));
        return -1;
    }
    if ((unsigned long)st.f_type != (unsigned long)NSFS_MAGIC) {
        // File exists (for example, a leftover placeholder after upgrading from
        // an older pod) but is not an active nsfs mount; stay in current mntns.
        close(fd);
        return 0;
    }

    char cwd[1024];
    if (getcwd(cwd, sizeof(cwd)) == NULL) {
        fprintf(stderr, "getcwd() failed\n");
        close(fd);
        return 0;
    }

    if (setns(fd, CLONE_NEWNS) == -1) {
        int err = errno;
        close(fd);
        fprintf(stderr, "%d:%d setns on mnt namespace failed: %s\n", getppid(), getpid(), strerror(err));
        return -1;
    }
    close(fd);

    // Restore original cwd after switching mount namespace.
    if (chdir(cwd) != 0) {
        fprintf(stderr, "chdir() failed\n");
    }

    return 0;
}

void enter_namespace(void) {
    if (getenv("CUBEMNT") == NULL) {
        return;
    }
    if (enter_namespace_path(CUBELET_MNT_NS_PATH) == -1) {
        exit(-1);
    }
}
