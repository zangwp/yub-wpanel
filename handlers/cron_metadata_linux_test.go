//go:build linux

package handlers

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteWPConfigAtomicallyPreservesExtendedAttributes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wp-config.php")
	original := []byte("<?php\n// before\n")
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	attributeName := "user.yub_wpanel_test"
	attributeValue := []byte("preserve-me")
	if err := syscall.Setxattr(path, attributeName, attributeValue, 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EPERM) {
			t.Skipf("extended attributes unavailable: %v", err)
		}
		t.Fatal(err)
	}

	if err := writeWPConfigAtomically(path, original, []byte("<?php\n// after\n")); err != nil {
		t.Fatal(err)
	}
	size, err := syscall.Getxattr(path, attributeName, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, size)
	if _, err := syscall.Getxattr(path, attributeName, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, attributeValue) {
		t.Fatalf("extended attribute=%q, want %q", got, attributeValue)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode=%o, want 0640", info.Mode().Perm())
	}
}
