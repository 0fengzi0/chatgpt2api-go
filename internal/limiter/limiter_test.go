package limiter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
)

func TestLimiterQueueAndDynamicCapacity(t *testing.T) {
	cfg := config.Default()
	cfg.Performance.ImageWorkers = 1
	cfg.Image.QueueSize = 1
	store := config.NewStore(t.TempDir()+"/config.yaml", &cfg)
	manager := New(store)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	releaseFirst, err := manager.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondReady := make(chan func(), 1)
	go func() {
		release, err := manager.Acquire(ctx)
		if err != nil {
			secondReady <- nil
			return
		}
		secondReady <- release
	}()
	deadline := time.Now().Add(time.Second)
	for {
		active, waiting := manager.Stats()
		if active == 1 && waiting == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unexpected limiter state active=%d waiting=%d", active, waiting)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := manager.Acquire(ctx); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected queue full, got %v", err)
	}
	releaseFirst()
	select {
	case releaseSecond := <-secondReady:
		if releaseSecond == nil {
			t.Fatal("second acquire failed")
		}
		releaseSecond()
	case <-ctx.Done():
		t.Fatal("second acquire timed out")
	}

	if err := store.Update(func(next *config.Config) { next.Performance.ImageWorkers = 2 }); err != nil {
		t.Fatal(err)
	}
	releaseA, err := manager.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	releaseB, err := manager.Acquire(ctx)
	if err != nil {
		releaseA()
		t.Fatal(err)
	}
	releaseB()
	releaseA()
}
