package prowlarr

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

func TestShouldRetry(t *testing.T) {
	transient := fmt.Errorf("blip: %w", clienterr.ErrTransient)
	cases := []struct {
		name    string
		err     error
		elapsed time.Duration
		want    bool
	}{
		{"fast transient retries", transient, 1 * time.Second, true},
		{"slow transient does not", transient, prowlarrRetryFastFail + time.Second, false},
		{"rejected never retries", fmt.Errorf("x: %w", clienterr.ErrRejected), 1 * time.Millisecond, false},
		{"not-found never retries", fmt.Errorf("x: %w", clienterr.ErrNotFound), 1 * time.Millisecond, false},
		{"success never retries", nil, 1 * time.Millisecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetry(tc.err, tc.elapsed); got != tc.want {
				t.Fatalf("shouldRetry(%v, %v) = %v, want %v", tc.err, tc.elapsed, got, tc.want)
			}
		})
	}
}

func TestTransientRetryLoop(t *testing.T) {
	// Shrink the backoff so the loop runs fast.
	defer func(d time.Duration) { prowlarrRetryBaseDelay = d }(prowlarrRetryBaseDelay)
	prowlarrRetryBaseDelay = time.Millisecond

	t.Run("retries a fast transient then succeeds", func(t *testing.T) {
		calls := 0
		err := transientRetry(context.Background(), func() error {
			calls++
			if calls < 2 {
				return fmt.Errorf("blip: %w", clienterr.ErrTransient)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("got %v, want nil after recovery", err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2 (one retry)", calls)
		}
	})

	t.Run("gives up after the attempt cap and returns the last error", func(t *testing.T) {
		calls := 0
		want := fmt.Errorf("still down: %w", clienterr.ErrTransient)
		err := transientRetry(context.Background(), func() error {
			calls++
			return want
		})
		if !errors.Is(err, clienterr.ErrTransient) {
			t.Fatalf("got %v, want a transient error", err)
		}
		if calls != prowlarrSearchAttempts {
			t.Fatalf("calls = %d, want %d (all attempts)", calls, prowlarrSearchAttempts)
		}
	})

	t.Run("does not retry a rejected error", func(t *testing.T) {
		calls := 0
		err := transientRetry(context.Background(), func() error {
			calls++
			return fmt.Errorf("400: %w", clienterr.ErrRejected)
		})
		if !errors.Is(err, clienterr.ErrRejected) {
			t.Fatalf("got %v, want rejected", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (no retry on rejected)", calls)
		}
	})

	t.Run("stops retrying when ctx is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := transientRetry(ctx, func() error {
			calls++
			return fmt.Errorf("blip: %w", clienterr.ErrTransient)
		})
		if !errors.Is(err, clienterr.ErrTransient) {
			t.Fatalf("got %v, want the last transient error", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (ctx cancelled before backoff completes)", calls)
		}
	})
}
