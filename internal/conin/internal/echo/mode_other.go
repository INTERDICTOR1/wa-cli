//go:build !windows

package main

// consoleModes exists only on Windows; elsewhere there is nothing to report.
func consoleModes() (in, out uint32) { return 0, 0 }
