//go:build !desktop

package main

import "context"

// nativeWindowBuilt reports whether this build can show its own window. The default build
// opens the user's browser instead; build with -tags desktop for the native window.
func nativeWindowBuilt() bool { return false }

func runWindow(context.Context, <-chan string) error { return nil }
