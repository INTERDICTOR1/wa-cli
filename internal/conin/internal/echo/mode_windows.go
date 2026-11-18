//go:build windows

package main

import "golang.org/x/sys/windows"

// consoleModes returns the current modes of the console's input and output,
// so the tests can check that they are put back the way they were.
func consoleModes() (in, out uint32) {
	if h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE); err == nil {
		_ = windows.GetConsoleMode(h, &in)
	}
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil {
		_ = windows.GetConsoleMode(h, &out)
	}
	return in, out
}
