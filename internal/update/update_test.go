package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1.1.0", "1.0.0", true},
		{"1.0.10", "1.0.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.1.0", false},
		{"v1.2.0", "1.1.0", true},
		{"1.2.0", "dev", false},
		{"", "1.0.0", false},
	} {
		if got := newer(tc.a, tc.b); got != tc.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func serve(t *testing.T, version string) *atomic.Int32 {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"name":"wa-terminal","version":"` + version + `"}`))
	}))
	t.Cleanup(srv.Close)
	old := registryURL
	registryURL = srv.URL
	t.Cleanup(func() { registryURL = old })
	return &hits
}

func TestNewerReportsUpdateAndCachesForADay(t *testing.T) {
	hits := serve(t, "1.2.0")
	dir := t.TempDir()
	if got := Newer(context.Background(), dir, "1.1.0"); got != "1.2.0" {
		t.Fatalf("Newer = %q, want 1.2.0", got)
	}
	if got := Newer(context.Background(), dir, "1.1.0"); got != "1.2.0" {
		t.Fatalf("cached Newer = %q, want 1.2.0", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("registry asked %d times, want once a day", hits.Load())
	}
	if got := Newer(context.Background(), dir, "1.2.0"); got != "" {
		t.Fatalf("up to date: Newer = %q, want \"\"", got)
	}
}

func TestDevBuildsNeverCheck(t *testing.T) {
	hits := serve(t, "9.9.9")
	if got := Newer(context.Background(), t.TempDir(), "dev"); got != "" || hits.Load() != 0 {
		t.Fatalf("dev build: Newer = %q, %d requests; want no check", got, hits.Load())
	}
}
