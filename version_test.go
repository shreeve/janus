package janus

import "testing"

func TestVersionPrefersThePublishedOne(t *testing.T) {
	prev, _ := publishedVersion.Load().(string)
	t.Cleanup(func() { publishedVersion.Store(prev) })
	publishedVersion.Store("")
	if Version() == "" {
		t.Fatal("Version is empty with nothing published")
	}
	SetVersion(" v1.18.1 ")
	if got := Version(); got != "1.18.1" {
		t.Fatalf("Version = %q, want 1.18.1", got)
	}
}
