#define _GNU_SOURCE
#include <errno.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
void enter_namespace(void) {
  char *newMntPath;
  newMntPath = getenv("NEED_SET_MNT");
  if (newMntPath) {
      int fd = open(newMntPath, O_RDONLY);
      if (setns(fd, 0) == -1) {
	     fprintf(stderr, "%d:%d setns on mnt namespace failed: %s\n", getppid(),getpid(), strerror(errno));
         exit(-1);
      } else {
	     //fprintf(stdout, "%d:%d setns on mnt namespace succeeded\n",getppid(),getpid());
      }
  }else{
      //printf("enter_namespace nothing\n");
  }
  return;
}