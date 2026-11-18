//go:build !windows

package memusage

import (
	"bufio"
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
)

func current() (uint64, bool) {
	if rss, ok := linuxRSS(); ok {
		return rss, true
	}
	sample := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0, false
	}
	return sample[0].Value.Uint64(), true
}

// linuxRSS reads VmRSS from /proc/self/status.
func linuxRSS() (uint64, bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "VmRSS:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest) // "12345 kB"
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}
