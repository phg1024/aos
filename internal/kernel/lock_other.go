//go:build !unix

package kernel

// lockFile is a no-op where flock is unavailable: the store's in-memory
// mutex still serialises one process, but several processes sharing a state
// directory are not coordinated on these platforms.
func lockFile(path string) (func(), error) {
	return func() {}, nil
}
