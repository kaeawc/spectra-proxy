package main

import "testing"

func TestRunCallRejectsMissingTarget(t *testing.T) {
	if got := runCall([]string{"--operation", "health"}); got != 2 {
		t.Fatalf("runCall() = %d, want 2", got)
	}
}

func TestRunCallRejectsInvalidParams(t *testing.T) {
	if got := runCall([]string{"--target", "host:7878", "--operation", "health", "--params", "{"}); got != 2 {
		t.Fatalf("runCall() = %d, want 2", got)
	}
}
