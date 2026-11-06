#include <stdio.h>
#include <unistd.h>
#include <xpc/xpc.h>
#include <servers/bootstrap.h>
extern int xpc_pipe_routine(xpc_object_t, xpc_object_t, xpc_object_t *);
extern xpc_object_t xpc_pipe_create_from_port(mach_port_t, uint64_t);
int main(int argc, char **argv) {
  xpc_object_t pipe = xpc_pipe_create_from_port(bootstrap_port, 0);
  xpc_object_t d = xpc_dictionary_create(NULL, NULL, 0), reply = NULL;
  xpc_dictionary_set_uint64(d, "subsystem", 2);
  xpc_dictionary_set_uint64(d, "routine", 708);
  xpc_dictionary_set_uint64(d, "type", 8);
  xpc_dictionary_set_uint64(d, "handle", getuid());
  xpc_dictionary_set_string(d, "name", argv[1]);
  xpc_dictionary_set_fd(d, "fd", 1);
  int rc = xpc_pipe_routine(pipe, d, &reply);
  fprintf(stderr, "rc %d error %lld\n", rc, reply ? xpc_dictionary_get_int64(reply, "error") : -1);
  return 0;
}
