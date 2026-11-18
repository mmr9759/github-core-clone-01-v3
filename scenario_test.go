package main

import "testing"

func TestViewerLogin(t *testing.T) {
	if got := viewerLogin(); got != "adaiversen" {
		t.Fatalf("viewerLogin() = %q, want adaiversen", got)
	}
	r := newRng(scenarioSeedFor("users"))
	users := generateUsers(r, 12)
	if len(users) != 12 {
		t.Fatalf("generateUsers returned %d users", len(users))
	}
	if users[1].login != "quinnkeller" {
		t.Fatalf("users[1] = %q, want quinnkeller", users[1].login)
	}
	if users[2].login != "olgabauer" {
		t.Fatalf("users[2] = %q, want olgabauer", users[2].login)
	}
	if users[0].id != 1000 || users[11].id != 1011 {
		t.Fatalf("user ids wrong: %d..%d", users[0].id, users[11].id)
	}
}

func TestRngDeterminism(t *testing.T) {
	a := newRng(scenarioSeedFor("users"))
	b := newRng(scenarioSeedFor("users"))
	for i := 0; i < 100; i++ {
		if a.nextU64() != b.nextU64() {
			t.Fatal("rng streams diverged")
		}
	}
}
