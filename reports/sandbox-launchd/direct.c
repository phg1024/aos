#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <xpc/xpc.h>
#include <servers/bootstrap.h>
extern int xpc_pipe_routine(xpc_object_t, xpc_object_t, xpc_object_t *);
extern xpc_object_t xpc_pipe_create_from_port(mach_port_t, uint64_t);
int main(int argc, char **argv) {
  xpc_object_t pipe = xpc_pipe_create_from_port(bootstrap_port, 0);
  xpc_object_t d = xpc_dictionary_create(NULL, NULL, 0), reply = NULL;
  xpc_dictionary_set_uint64(d, "subsystem", 3);
  xpc_dictionary_set_uint64(d, "type", 8);
  xpc_dictionary_set_uint64(d, "handle", getuid());
  if (argc > 1) {
    xpc_object_t names = xpc_array_create(NULL, 0);
    xpc_array_set_string(names, XPC_ARRAY_APPEND, argv[1]);
    xpc_dictionary_set_value(d, "names", names);
    xpc_dictionary_set_string(d, "name", argv[1]);
    xpc_dictionary_set_uint64(d, "routine", 809);
  } else {
    xpc_dictionary_set_uint64(d, "routine", 815);
    xpc_dictionary_set_bool(d, "legacy", 1);
  }
  int rc = xpc_pipe_routine(pipe, d, &reply);
  printf("rc %d\n", rc);
  if (!reply) return 1;
  printf("error %lld\n", xpc_dictionary_get_int64(reply, "error"));
  xpc_object_t services = xpc_dictionary_get_value(reply, "services");
  if (services) printf("services %zu\n", xpc_dictionary_get_count(services));
  char *text = xpc_copy_description(reply);
  printf("%.200s\n", text);
  return 0;
}
