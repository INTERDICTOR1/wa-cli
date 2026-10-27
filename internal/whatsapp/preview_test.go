package whatsapp

import "testing"

func TestDecodeCost_Budget(t *testing.T) {
	cases := []struct {
		name         string
		w, h, size   int
		jpeg, within bool
	}{
		{"WhatsApp standard JPEG", 1600, 1600, 300 << 10, true, true},
		{"WhatsApp HD JPEG", 4096, 3072, 11 << 20, true, true},
		{"2048 PNG", 2048, 2048, 17 << 20, false, true},
		{"4096 PNG", 4096, 4096, 67 << 20, false, false},
		{"huge JPEG", 8000, 6000, 20 << 20, true, false},
	}
	for _, c := range cases {
		if got := decodeCost(c.w, c.h, c.size, c.jpeg) <= previewMemoryBudget; got != c.within {
			t.Errorf("%s: within budget = %v, want %v", c.name, got, c.within)
		}
	}
}
