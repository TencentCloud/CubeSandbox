// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

#define _GNU_SOURCE

#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/reboot.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <linux/vm_sockets.h>
#include <unistd.h>

#define CONTROL_PORT 1024U
#define ENVD_PORT 49983U
#define EXEC_PORT 49999U
#define BRIDGE_BUFFER_SIZE 65536U

static void fail(const char *operation) {
  fprintf(stderr, "CUBEVZ_AGENT_ERROR %s: %s\n", operation, strerror(errno));
  fflush(stderr);
  exit(1);
}

static int write_bytes(int fd, const void *data, size_t size) {
  const char *cursor = data;
  size_t remaining = size;
  while (remaining > 0) {
    ssize_t written = write(fd, cursor, remaining);
    if (written < 0) {
      if (errno == EINTR) {
        continue;
      }
      return -1;
    }
    if (written == 0) {
      return -1;
    }
    cursor += written;
    remaining -= (size_t)written;
  }
  return 0;
}

static void write_all(int fd, const char *message) {
  (void)write_bytes(fd, message, strlen(message));
}

static int connect_service(unsigned short port) {
  int fd = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (fd < 0) {
    return -1;
  }
  struct sockaddr_in address = {
      .sin_family = AF_INET,
      .sin_port = htons(port),
      .sin_addr = {.s_addr = htonl(INADDR_LOOPBACK)},
  };
  if (connect(fd, (struct sockaddr *)&address, sizeof(address)) < 0) {
    close(fd);
    return -1;
  }
  return fd;
}

static void wait_for_service(unsigned short port) {
  for (;;) {
    int fd = connect_service(port);
    if (fd >= 0) {
      close(fd);
      return;
    }
    usleep(5000);
  }
}

static void bridge_descriptors(int left, int right) {
  int left_open = 1;
  int right_open = 1;
  char buffer[BRIDGE_BUFFER_SIZE];

  while (left_open || right_open) {
    struct pollfd descriptors[2] = {
        {.fd = left, .events = left_open ? POLLIN : 0},
        {.fd = right, .events = right_open ? POLLIN : 0},
    };
    int ready = poll(descriptors, 2, -1);
    if (ready < 0) {
      if (errno == EINTR) {
        continue;
      }
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
      if (count <= 0) {
        break;
      }
      if (write_bytes(left, buffer, (size_t)count) < 0) {
        break;
      }
    }
  }
}

struct client_config {
  int descriptor;
  unsigned short port;
};

static pthread_mutex_t bridge_lock = PTHREAD_MUTEX_INITIALIZER;
static unsigned short bridged_ports[256];
static size_t bridged_port_count = 0;

static void *bridge_service_client(void *argument) {
  struct client_config config = *(struct client_config *)argument;
  free(argument);
  int tcp = connect_service(config.port);
  if (tcp >= 0) {
    bridge_descriptors(config.descriptor, tcp);
    close(tcp);
  }
  close(config.descriptor);
  return NULL;
}

static void *serve_service_bridge(void *argument) {
  unsigned short port = *(unsigned short *)argument;
  free(argument);
  int server = socket(AF_VSOCK, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (server < 0) {
    fail("service bridge socket");
  }
  struct sockaddr_vm address = {
      .svm_family = AF_VSOCK,
      .svm_port = port,
      .svm_cid = VMADDR_CID_ANY,
  };
  if (bind(server, (struct sockaddr *)&address, sizeof(address)) < 0) {
    fail("service bridge bind");
  }
  if (listen(server, 128) < 0) {
    fail("service bridge listen");
  }

  for (;;) {
    int client = accept4(server, NULL, NULL, SOCK_CLOEXEC);
    if (client < 0) {
      if (errno == EINTR) {
        continue;
      }
      fail("service bridge accept");
    }
    struct client_config *argument = malloc(sizeof(*argument));
    if (argument == NULL) {
      close(client);
      continue;
    }
    argument->descriptor = client;
    argument->port = port;
    pthread_t thread;
    if (pthread_create(&thread, NULL, bridge_service_client, argument) != 0) {
      free(argument);
      close(client);
      continue;
    }
    pthread_detach(thread);
  }

  return NULL;
}

static int start_service_bridge(unsigned short port) {
  pthread_mutex_lock(&bridge_lock);
  for (size_t index = 0; index < bridged_port_count; ++index) {
    if (bridged_ports[index] == port) {
      pthread_mutex_unlock(&bridge_lock);
      return 0;
    }
  }
  if (bridged_port_count >= sizeof(bridged_ports) / sizeof(bridged_ports[0])) {
    pthread_mutex_unlock(&bridge_lock);
    return -1;
  }
  unsigned short *argument = malloc(sizeof(*argument));
  if (argument == NULL) {
    pthread_mutex_unlock(&bridge_lock);
    return -1;
  }
  *argument = port;
  pthread_t thread;
  if (pthread_create(&thread, NULL, serve_service_bridge, argument) != 0) {
    free(argument);
    pthread_mutex_unlock(&bridge_lock);
    return -1;
  }
  pthread_detach(thread);
  bridged_ports[bridged_port_count++] = port;
  pthread_mutex_unlock(&bridge_lock);
  return 0;
}

static int run_script(const char *path) {
  pid_t child = fork();
  if (child < 0) {
    return -1;
  }
  if (child == 0) {
    execl("/bin/sh", "sh", path, (char *)NULL);
    _exit(127);
  }
  int status = 0;
  while (waitpid(child, &status, 0) < 0) {
    if (errno != EINTR) {
      return -1;
    }
  }
  return WIFEXITED(status) && WEXITSTATUS(status) == 0 ? 0 : -1;
}

static int run_program(const char *path) {
  pid_t child = fork();
  if (child < 0) {
    return -1;
  }
  if (child == 0) {
    execl(path, path, (char *)NULL);
    _exit(127);
  }
  int status = 0;
  while (waitpid(child, &status, 0) < 0) {
    if (errno != EINTR) {
      return -1;
    }
  }
  return WIFEXITED(status) && WEXITSTATUS(status) == 0 ? 0 : -1;
}

int main(void) {
  signal(SIGPIPE, SIG_IGN);
  wait_for_service(ENVD_PORT);
  wait_for_service(EXEC_PORT);
  if (start_service_bridge(ENVD_PORT) < 0 || start_service_bridge(EXEC_PORT) < 0) {
    fail("service bridge thread");
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

    write_all(client, "READY\n");
    for (;;) {
      char request[64] = {0};
      ssize_t count = read(client, request, sizeof(request) - 1);
      if (count <= 0) {
        break;
      }
      if (strncmp(request, "PING", 4) == 0) {
        write_all(client, "READY\n");
      } else if (strncmp(request, "APPLY_NETWORK", 13) == 0) {
        write_all(client, run_script("/run/cube-vz-network.sh") == 0 ? "OK\n" : "ERROR\n");
      } else if (strncmp(request, "APPLY_VOLUMES", 13) == 0) {
        write_all(client, run_script("/run/cube-vz-volumes.sh") == 0 ? "OK\n" : "ERROR\n");
      } else if (strncmp(request, "START_TEMPLATE", 14) == 0) {
        write_all(client, run_program("/usr/local/sbin/cube-vz-template-entrypoint") == 0 ? "OK\n" : "ERROR\n");
      } else if (strncmp(request, "FORWARD ", 8) == 0) {
        char *end = NULL;
        unsigned long parsed = strtoul(request + 8, &end, 10);
        if (end != NULL) {
          while (*end == '\n' || *end == '\r' || *end == ' ') ++end;
        }
        if (parsed == 0 || parsed > 65535UL || end == NULL || *end != '\0') {
          write_all(client, "ERROR\n");
        } else {
          unsigned short port = (unsigned short)parsed;
          int probe = connect_service(port);
          if (probe < 0 || start_service_bridge(port) < 0) {
            if (probe >= 0) close(probe);
            write_all(client, "ERROR\n");
          } else {
            close(probe);
            write_all(client, "OK\n");
          }
        }
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
