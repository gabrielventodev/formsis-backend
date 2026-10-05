package storage

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func TestS3(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	defer srv.Close()

	s, err := NewS3(ctx, Config{
		Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "formflow",
		AccessKey: "k", SecretKey: "s", Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "org/sub/a.pdf", strings.NewReader("%PDF"), 4, "application/pdf"); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(ctx, "org/sub/a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "%PDF" {
		t.Fatalf("got %q", b)
	}
	if err := s.Delete(ctx, "org/sub/a.pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, "org/sub/a.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// Creating the store again finds the existing bucket.
	if _, err := NewS3(ctx, Config{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "formflow", AccessKey: "k", SecretKey: "s", Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
}
