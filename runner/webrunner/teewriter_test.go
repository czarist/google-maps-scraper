package webrunner

import (
	"context"
	"sync"
	"testing"

	"github.com/gosom/scrapemate"
)

type countingWriter struct {
	mu   sync.Mutex
	seen []any
}

func (c *countingWriter) Run(_ context.Context, in <-chan scrapemate.Result) error {
	for r := range in {
		c.mu.Lock()
		c.seen = append(c.seen, r.Data)
		c.mu.Unlock()
	}

	return nil
}

func TestTeeWriterSendsEveryResultToEveryWriter(t *testing.T) {
	const total = 100

	a, b := &countingWriter{}, &countingWriter{}
	tee := newTeeWriter(a, b)

	in := make(chan scrapemate.Result)

	go func() {
		for i := range total {
			in <- scrapemate.Result{Data: i}
		}

		close(in)
	}()

	if err := tee.Run(context.Background(), in); err != nil {
		t.Fatalf("tee run: %v", err)
	}

	if len(a.seen) != total || len(b.seen) != total {
		t.Fatalf("got %d and %d results, want %d each", len(a.seen), len(b.seen), total)
	}
}

func TestTeeWriterKeepsForwardingAfterCancel(t *testing.T) {
	const total = 20

	a, b := &countingWriter{}, &countingWriter{}
	tee := newTeeWriter(a, b)

	// The job context is canceled when scraping ends; results still queued
	// at that point must reach every writer.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	in := make(chan scrapemate.Result, total)
	for i := range total {
		in <- scrapemate.Result{Data: i}
	}

	close(in)

	if err := tee.Run(ctx, in); err != nil {
		t.Fatalf("tee run: %v", err)
	}

	if len(a.seen) != total || len(b.seen) != total {
		t.Fatalf("got %d and %d results, want %d each", len(a.seen), len(b.seen), total)
	}
}

func TestNewTeeWriterWithOneWriterReturnsIt(t *testing.T) {
	w := &countingWriter{}

	if got := newTeeWriter(w); got != w {
		t.Fatal("a single writer should be used directly")
	}
}
