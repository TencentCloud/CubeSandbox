// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

#define _GNU_SOURCE

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/reboot.h>
#include <sys/socket.h>
#include <linux/vm_sockets.h>
#include <unistd.h>

#define CONTROL_PORT 1024U

static void fail(const char *operation) {
  fprintf(stderr, "CUBEVZ_AGENT_ERROR %s: %s\n", operation, strerror(errno));
  fflush(stderr);
  exit(1);
}

static void write_all(int fd, const char *message) {
  size_t remaining = strlen(message);
  while (remaining > 0) {
    ssize_t written = write(fd, message, remaining);
    if (written < 0) {
      if (errno == EINTR) {
        continue;
      }
      return;
    }
    message += written;
    remaining -= (size_t)written;
  }
}

static ssize_t read_line(int fd, char *buffer, size_t capacity) {
  size_t used = 0;
  while (used + 1 < capacity) {
    ssize_t count = read(fd, buffer + used, capacity - used - 1);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) return count;
    used += (size_t)count;
    buffer[used] = '\0';
    if (memchr(buffer, '\n', used) != NULL) return (ssize_t)used;
  }
  errno = EMSGSIZE;
  return -1;
}

int main(int argc, char **argv) {
  char ready_message[256];
  if (argc > 1) {
    snprintf(ready_message, sizeof(ready_message), "READY %s\n", argv[1]);
  } else {
    snprintf(ready_message, sizeof(ready_message), "READY\n");
  }
  printf("CUBEVZ_LIFECYCLE_READY port=%u\n", CONTROL_PORT);
  fflush(stdout);

  for (;;) {
    int client = socket(AF_VSOCK, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (client < 0) {
      fail("socket");
    }

    struct sockaddr_vm address = {
        .svm_family = AF_VSOCK,
        .svm_port = CONTROL_PORT,
        .svm_cid = VMADDR_CID_HOST,
    };
    while (connect(client, (struct sockaddr *)&address, sizeof(address)) < 0) {
      if (errno == EINTR) {
        continue;
      }
      close(client);
      usleep(5000);
      client = socket(AF_VSOCK, SOCK_STREAM | SOCK_CLOEXEC, 0);
      if (client < 0) {
        fail("socket");
      }
    }

    write_all(client, ready_message);
    for (;;) {
      char request[64] = {0};
      ssize_t count = read_line(client, request, sizeof(request));
      if (count <= 0) {
        break;
      }
      if (strncmp(request, "PING", 4) == 0) {
        write_all(client, ready_message);
      } else if (strncmp(request, "SHUTDOWN", 8) == 0) {
        write_all(client, "OK\n");
        close(client);
        sync();
        if (reboot(RB_POWER_OFF) < 0) {
          fail("poweroff");
        }
        return 0;
      } else {
        write_all(client, "ERROR\n");
      }
    }
    close(client);
  }
}
