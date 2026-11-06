#include <mach/mach.h>
#include <stdio.h>
#include <unistd.h>
int main(int argc, char **argv) {
  kern_return_t kr = task_set_special_port(mach_task_self(), TASK_BOOTSTRAP_PORT, MACH_PORT_NULL);
  if (kr != KERN_SUCCESS) { fprintf(stderr, "task_set_special_port %d\n", kr); return 125; }
  execv(argv[1], argv + 1);
  perror("execv");
  return 126;
}
