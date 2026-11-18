//go:build !desktop

package main

// newNativeUI reports that this build has no window of its own: the default build opens the
// user's browser instead. Build with -tags desktop for the native window.
func newNativeUI() nativeUI { return nil }
