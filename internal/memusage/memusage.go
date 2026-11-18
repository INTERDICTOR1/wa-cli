// Package memusage reports how much RAM this process is using, as the
// operating system counts it - including what lives outside the Go heap
// (SQLite's cache, the program's code) - so the figure can be compared
// with Task Manager or top.
package memusage

// Current returns the RAM this process uses right now, in bytes, and
// whether it could be measured.
//
// On Windows it is the private working set: the RAM used by this process
// alone, which is what Task Manager's "Memory" column shows. On Linux it
// is the resident set (VmRSS). Elsewhere it is the memory the Go runtime
// holds, which leaves out memory used outside Go.
func Current() (uint64, bool) { return current() }
