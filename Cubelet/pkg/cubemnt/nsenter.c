#define _GNU_SOURCE
#include <errno.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
void enter_namespace(void) {
    if (getenv("CUBEMNT") == NULL) {
        return;
    }
    int fd = open("/usr/local/services/cubetoolbox/cubeletmnt/mnt", O_RDONLY);
    char cwd[1024];
    if (getcwd(cwd, sizeof(cwd)) == NULL) {
        fprintf(stderr, "getcwd() failed\n");
        return ;
    }

    if (setns(fd, 0) == -1) {
        fprintf(stderr, "%d:%d setns on mnt namespace failed: %s\n", getppid(), getpid(), strerror(errno));
        exit(-1);
    }

    // change to origin cwd
    if (chdir(cwd) != 0) {
        fprintf(stderr, "chdir() failed\n");
        return;
    }

    return;
}
