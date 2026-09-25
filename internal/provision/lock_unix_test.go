//go:build unix

package provision

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMutatingLockContentionFailsFast(t *testing.T) {
	s, priv := newTestServer(t)
	s.releases["v1.0.0"] = fixture(t, "v1.0.0", priv, nil)
	unlock, err := prepareRoot(s.root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	_, err = Install(context.Background(), s.opts(), "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "another provisioning operation is in progress") {
		t.Fatalf("lock error: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("lock blocked for %s", time.Since(start))
	}
}
