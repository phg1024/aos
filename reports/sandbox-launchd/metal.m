#import <Metal/Metal.h>
#include <stdio.h>
int main(void) {
  id<MTLDevice> d = MTLCreateSystemDefaultDevice();
  if (!d) { puts("no device"); return 1; }
  NSError *e = nil;
  id<MTLLibrary> l = [d newLibraryWithSource:@"kernel void k(){}" options:nil error:&e];
  printf("device %s, compiled %s\n", d.name.UTF8String, l ? "yes" : e.localizedDescription.UTF8String);
  return l ? 0 : 1;
}
