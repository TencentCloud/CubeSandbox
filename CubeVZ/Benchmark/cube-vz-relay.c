// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

#define _GNU_SOURCE

#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/vm_sockets.h>
#include <netinet/in.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#define BUFFER_SIZE 65536U

struct client_config {
  int descriptor;
  uint16_t port;
};

static int write_bytes(int descriptor, const void *data, size_t size) {
  const char *cursor = data;
  while (size > 0) {
    ssize_t count = write(descriptor, cursor, size);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) return -1;
    cursor += count;
    size -= (size_t)count;
  }
  return 0;
}

static int connect_loopback(uint16_t port) {
  int descriptor = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (descriptor < 0) return -1;
  struct sockaddr_in address = {
      .sin_family = AF_INET,
      .sin_port = htons(port),
      .sin_addr = {.s_addr = htonl(INADDR_LOOPBACK)},
  };
  if (connect(descriptor, (struct sockaddr *)&address, sizeof(address)) < 0) {
    close(descriptor);
    return -1;
  }
  return descriptor;
}

static void bridge(int left, int right) {
  int left_open = 1;
  int right_open = 1;
  char buffer[BUFFER_SIZE];
  while (left_open || right_open) {
    struct pollfd descriptors[2] = {
        {.fd = left, .events = left_open ? POLLIN : 0},
        {.fd = right, .events = right_open ? POLLIN : 0},
    };
    int ready = poll(descriptors, 2, -1);
    if (ready < 0) {
      if (errno == EINTR) continue;
      break;
    }
    if (left_open && (descriptors[0].revents & (POLLIN | POLLHUP | POLLERR))) {
      ssize_t count = read(left, buffer, sizeof(buffer));
      if (count <= 0 || write_bytes(right, buffer, (size_t)count) < 0) {
        left_open = 0;
        shutdown(right, SHUT_WR);
      }
    }
    if (right_open && (descriptors[1].revents & (POLLIN | POLLHUP | POLLERR))) {
      ssize_t count = read(right, buffer, sizeof(buffer));
      if (count <= 0 || write_bytes(left, buffer, (size_t)count) < 0) {
        right_open = 0;
        shutdown(left, SHUT_WR);
      }
    }
  }
}

static void *serve_client(void *argument) {
  struct client_config config = *(struct client_config *)argument;
  free(argument);
  int service = connect_loopback(config.port);
  if (service >= 0) {
    bridge(config.descriptor, service);
    close(service);
  }
  close(config.descriptor);
  return NULL;
}

int main(int argc, char **argv) {
  if (argc < 2 || argc > 3) {
    fprintf(stderr, "usage: cube-vz-relay PORT [READY_FILE]\n");
    return 2;
  }
  char *end = NULL;
  unsigned long parsed = strtoul(argv[1], &end, 10);
  if (end == argv[1] || *end != '\0' || parsed == 0 || parsed > UINT16_MAX) {
    fprintf(stderr, "cube-vz-relay: invalid port: %s\n", argv[1]);
    return 2;
  }
  uint16_t port = (uint16_t)parsed;
  signal(SIGPIPE, SIG_IGN);

  int server = socket(AF_VSOCK, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (server < 0) {
    perror("cube-vz-relay: socket");
    return 1;
  }
  struct sockaddr_vm address = {
      .svm_family = AF_VSOCK,
      .svm_port = port,
      .svm_cid = VMADDR_CID_ANY,
  };
  if (bind(server, (struct sockaddr *)&address, sizeof(address)) < 0 ||
      listen(server, 128) < 0) {
    perror("cube-vz-relay: listen");
    close(server);
    return 1;
  }
  if (argc == 3) {
    int ready = open(argv[2], O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
    if (ready < 0 || write_bytes(ready, "READY\n", 6) < 0) {
      perror("cube-vz-relay: ready file");
      if (ready >= 0) close(ready);
      close(server);
      return 1;
    }
    close(ready);
  }

  for (;;) {
    int client = accept4(server, NULL, NULL, SOCK_CLOEXEC);
    if (client < 0) {
      if (errno == EINTR) continue;
      perror("cube-vz-relay: accept");
      return 1;
    }
    struct client_config *config = malloc(sizeof(*config));
    if (config == NULL) {
      close(client);
      continue;
    }
    config->descriptor = client;
    config->port = port;
    pthread_t thread;
    if (pthread_create(&thread, NULL, serve_client, config) != 0) {
      free(config);
      close(client);
      continue;
    }
    pthread_detach(thread);
  }
}
