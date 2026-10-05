package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLocal(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "a/b/c.txt", strings.NewReader("hola"), 4, "text/plain"); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(ctx, "a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hola" {
		t.Fatalf("got %q", b)
	}
	if err := s.Delete(ctx, "a/b/c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, "a/b/c.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Put(ctx, "../escape", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("path traversal accepted")
	}
}
