// Package update tells you when a newer wa is published on npm.
//
// npm never updates a global package by itself, and WhatsApp refuses
// clients that fall too far behind, so wa checks the registry at most
// once a day and says when to run InstallCommand.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Package is the npm package wa is published as.
const Package = "wa-terminal"

// InstallCommand installs or updates wa.
const InstallCommand = "npm i -g " + Package

const checkEvery = 24 * time.Hour

// registryURL is the npm registry document for the latest release.
// A variable so tests can point it at a local server.
var registryURL = "https://registry.npmjs.org/" + Package + "/latest"

type cache struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

// Newer returns the latest published version when it is newer than
// current, or "". The answer is cached in dataDir for a day. Development
// builds (current not a version number) never check. Network problems
// are ignored: this is a hint, not a requirement.
func Newer(ctx context.Context, dataDir, current string) string {
	if _, ok := parse(current); !ok {
		return ""
	}
	path := filepath.Join(dataDir, "update-check.json")
	var c cache
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if time.Since(c.Checked) > checkEvery {
		if latest, err := fetchLatest(ctx); err == nil {
			c = cache{Checked: time.Now(), Latest: latest}
			if b, err := json.Marshal(c); err == nil {
				_ = os.WriteFile(path, b, 0o600)
			}
		}
	}
	if newer(c.Latest, current) {
		return c.Latest
	}
	return ""
}

func fetchLatest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry: %s", resp.Status)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	return doc.Version, nil
}

// newer reports whether version a is higher than b.
func newer(a, b string) bool {
	pa, ok1 := parse(a)
	pb, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

// parse reads "1.2.3" (a leading "v" and any "-pre" suffix allowed).
func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
