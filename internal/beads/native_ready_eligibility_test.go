package beads

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeReadyFallbackReasonRespectsSelectedOperatorAndHooks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "1")
	if got := NativeReadyFallbackReason(root, "bd", nil); got != "" {
		t.Fatalf("unselected ambient flag leaked: %s", got)
	}
	if got := NativeReadyFallbackReason(root, "bd", map[string]string{"GC_BEADS_FORCE_FALLBACK": "true"}); got == "" {
		t.Fatal("operator fallback ignored")
	}
	if got := NativeReadyFallbackReason(root, "file", nil); got == "" {
		t.Fatal("file provider accepted")
	}
	hook := filepath.Join(root, ".beads/hooks/on_update")
	if err := os.MkdirAll(filepath.Dir(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncustom-hook\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := NativeReadyFallbackReason(root, "bd", nil); got == "" {
		t.Fatal("custom hook ignored")
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n# gc-hook-stamp: test\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := NativeReadyFallbackReason(root, "bd", nil); got != "" {
		t.Fatalf("GC hook declined: %s", got)
	}
}
