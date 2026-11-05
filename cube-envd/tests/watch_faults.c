// SPDX-License-Identifier: Apache-2.0
// Test-only LD_PRELOAD syscall boundary injection for the dynamically linked
// development binary. Never linked into envd or the musl development image.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/inotify.h>
#include <sys/random.h>
#include <unistd.h>

ssize_t getrandom(void *buffer, size_t size, unsigned flags) {
    ssize_t (*next)(void *, size_t, unsigned) = dlsym(RTLD_NEXT, "getrandom");
    if (size == 16 && flags == GRND_NONBLOCK) {
        if (!access("/tmp/poll-id-hold", F_OK)) {
            int marker = open("/tmp/poll-id-entered", O_WRONLY | O_CREAT, 0600);
            if (marker >= 0) close(marker);
            for (int i=0; i<10000 && access("/tmp/poll-id-release", F_OK); ++i) usleep(1000);
        }
        if (!access("/tmp/poll-id-repeat", F_OK)) {
            memset(buffer, 42, size);
            return size;
        }
    }
    return next(buffer, size, flags);
}

int inotify_add_watch(int fd, const char *path, unsigned mask) {
    int (*next)(int, const char *, unsigned) = dlsym(RTLD_NEXT, "inotify_add_watch");
    char target[4096] = {0};
    ssize_t n = readlink(path, target, sizeof(target)-1);
    if (n > 0 && strstr(target, ".watch-eacces")) { errno = EACCES; return -1; }
    if (n > 0 && strstr(target, ".watch-gone")) { errno = ENOENT; return -1; }
    if (n > 0 && strstr(target, ".watch-enospc")) { errno = ENOSPC; return -1; }
    if (n > 0 && strstr(target, ".watch-hold")) {
        int marker = open("/tmp/watch-entered", O_WRONLY | O_CREAT, 0600);
        if (marker >= 0) close(marker);
        for (int i=0; i<10000 && access("/tmp/watch-release", F_OK); ++i) usleep(1000);
    }
    return next(fd, path, mask);
}

ssize_t read(int fd, void *buffer, size_t size) {
    ssize_t (*next)(int, void *, size_t) = dlsym(RTLD_NEXT, "read");
    ssize_t count = next(fd, buffer, size);
    if (count <= 0) return count;
    char path[64], target[64] = {0};
    snprintf(path, sizeof(path), "/proc/self/fd/%d", fd);
    if (readlink(path, target, sizeof(target)-1) < 0 || strcmp(target, "anon_inode:inotify")) return count;
    for (size_t offset=0; offset + sizeof(struct inotify_event) <= (size_t)count;) {
        struct inotify_event *event = (void *)((char *)buffer + offset);
        size_t length = sizeof(*event) + event->len;
        if (offset + length > (size_t)count) break;
        if (event->len && strstr(event->name, ".fault-kernel")) { event->wd=-1; event->mask=IN_Q_OVERFLOW; }
        if (event->len && strstr(event->name, ".fault-ignored")) event->mask=IN_IGNORED;
        if (event->len && strstr(event->name, ".fault-unmount")) event->mask=IN_UNMOUNT;
        if (event->len && strstr(event->name, ".fault-backend")) { errno=EIO; return -1; }
        if (event->len && strstr(event->name, ".fault-budget") && 514 * length <= size) {
            char original[512];
            if (length > sizeof(original)) abort();
            memcpy(original, event, length);
            for (int i=0; i<514; ++i) {
                struct inotify_event *item=(void *)((char *)buffer+i*length);
                memcpy(item, original, length);
                item->mask=(i<257 ? IN_MOVED_FROM : IN_MOVED_TO) | IN_ISDIR;
                item->cookie=(i%257)+1;
            }
            return 514 * length;
        }
        if (event->len && strstr(event->name, ".fault-correlation") && (size_t)count + length <= size) {
            event->mask=IN_MOVED_FROM | IN_ISDIR; event->cookie=0;
            struct inotify_event *pair=(void *)((char *)buffer+count);
            memcpy(pair,event,length); pair->mask=IN_MOVED_TO | IN_ISDIR;
            count += length;
            break;
        }
        offset += length;
    }
    return count;
}
