//go:build !unix

package trust

// lockPath is a no-op where advisory locks are unavailable; changes still re-read the file first.
func lockPath(string) (func(), error) { return func() {}, nil }
