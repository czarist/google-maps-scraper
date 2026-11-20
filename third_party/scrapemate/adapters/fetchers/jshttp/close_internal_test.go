package jshttp

import (
	"context"
	"errors"
	"testing"
)

func TestCloseWhileSlotIsHeldDoesNotPanic(t *testing.T) {
	t.Parallel()

	fetcher, _ := newJSFetchForTest(2)
	fetcher.closed = make(chan struct{})

	ctx := context.Background()

	held, err := fetcher.getSlot(ctx)
	if err != nil {
		t.Fatalf("getSlot returned error: %v", err)
	}

	if err := fetcher.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// A fetch that was running during Close gives its slot back afterwards.
	fetcher.putSlot(ctx, held)

	if n := len(fetcher.slots); n != 0 {
		t.Fatalf("closed fetcher kept %d slot(s) in its pool", n)
	}

	if _, err := fetcher.getSlot(ctx); !errors.Is(err, errFetcherClosed) {
		t.Fatalf("getSlot after Close = %v, want errFetcherClosed", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	fetcher, _ := newJSFetchForTest(1)
	fetcher.closed = make(chan struct{})

	done := make(chan struct{})

	for range 3 {
		go func() {
			_ = fetcher.Close()
			done <- struct{}{}
		}()
	}

	for range 3 {
		<-done
	}
}
