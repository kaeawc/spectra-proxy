//go:build !unix

package provision

import "testing"

func TestNonUnixLockStub(t *testing.T) {
	unlock, err := lock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
