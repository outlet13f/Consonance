package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitFor(t *testing.T) {
	t.Run("true on the third call", func(t *testing.T) {
		calls := 0
		start := time.Now()
		ok, err := waitFor(context.Background(), 5*time.Second, func(context.Context) (bool, error) {
			calls++
			return calls == 3, nil
		})
		if elapsed := time.Since(start); !ok || err != nil || calls != 3 || elapsed < 180*time.Millisecond || elapsed > 600*time.Millisecond {
			t.Fatalf("ok=%v err=%v calls=%d elapsed=%v, want true, nil, 3, about 200ms", ok, err, calls, elapsed)
		}
	})
	t.Run("never true", func(t *testing.T) {
		start := time.Now()
		ok, err := waitFor(context.Background(), 200*time.Millisecond, func(context.Context) (bool, error) { return false, nil })
		if elapsed := time.Since(start); ok || err != nil || elapsed < 190*time.Millisecond || elapsed > 600*time.Millisecond {
			t.Fatalf("ok=%v err=%v elapsed=%v, want false, nil, about 200ms", ok, err, elapsed)
		}
	})
	t.Run("error of done", func(t *testing.T) {
		boom := errors.New("boom")
		ok, err := waitFor(context.Background(), time.Second, func(context.Context) (bool, error) { return false, boom })
		if ok || !errors.Is(err, boom) {
			t.Fatalf("ok=%v err=%v, want false and boom", ok, err)
		}
	})
	t.Run("true at once", func(t *testing.T) {
		start := time.Now()
		ok, err := waitFor(context.Background(), time.Second, func(context.Context) (bool, error) { return true, nil })
		if elapsed := time.Since(start); !ok || err != nil || elapsed > 10*time.Millisecond {
			t.Fatalf("ok=%v err=%v elapsed=%v, want true, nil, under 10ms", ok, err, elapsed)
		}
	})
	t.Run("zero timeout still looks once", func(t *testing.T) {
		calls := 0
		ok, err := waitFor(context.Background(), 0, func(context.Context) (bool, error) { calls++; return false, nil })
		if ok || err != nil || calls != 1 {
			t.Fatalf("ok=%v err=%v calls=%d, want false, nil, 1", ok, err, calls)
		}
	})
	t.Run("request context ends", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		ok, err := waitFor(ctx, 5*time.Second, func(context.Context) (bool, error) { return false, nil })
		if ok || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Fatalf("ok=%v err=%v, want false and the context's error", ok, err)
		}
	})
}
