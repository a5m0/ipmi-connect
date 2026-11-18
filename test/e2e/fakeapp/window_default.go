//go:build !desktop

package main

import "errors"

// openWindow needs the desktop build: go build -tags desktop,purego,production ./test/e2e/fakeapp
func openWindow(string) error { return errors.New("FAKEAPP_WINDOW needs a build with -tags desktop") }
