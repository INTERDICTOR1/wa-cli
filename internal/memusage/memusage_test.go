package memusage

import "testing"

func TestCurrentIsPlausible(t *testing.T) {
	n, ok := Current()
	if !ok {
		t.Skip("memory use can't be measured here")
	}
	// A running Go test binary uses at least a megabyte and, here, far
	// less than a gigabyte.
	if n < 1<<20 || n > 1<<30 {
		t.Fatalf("Current() = %d bytes", n)
	}
	// Holding on to 32 MB more shows up.
	buf := make([]byte, 32<<20)
	for i := range buf {
		buf[i] = byte(i)
	}
	after, _ := Current()
	if after < n+16<<20 {
		t.Errorf("after touching 32 MB: %d -> %d bytes", n, after)
	}
	_ = buf[len(buf)-1]
}
