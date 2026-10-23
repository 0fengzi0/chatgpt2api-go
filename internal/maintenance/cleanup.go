package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/image"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

type Cleaner struct {
	db      *store.Store
	config  *config.Store
	storage *image.Storage
	logger  *slog.Logger
}

func New(db *store.Store, cfg *config.Store, storage *image.Storage, logger *slog.Logger) *Cleaner {
	return &Cleaner{db: db, config: cfg, storage: storage, logger: logger}
}

func (c *Cleaner) Start(ctx context.Context) {
	go func() {
		timer := time.NewTimer(10 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				removed, err := c.RunOnce(ctx)
				if err != nil {
					c.logger.Warn("image cleanup", "error", err)
				} else if removed > 0 {
					c.logger.Info("image cleanup complete", "removed", removed)
				}
				timer.Reset(6 * time.Hour)
			}
		}
	}()
}

func (c *Cleaner) RunOnce(ctx context.Context) (int, error) {
	cfg := c.config.Snapshot()
	if cfg.Image.RetentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(cfg.Image.RetentionDays) * 24 * time.Hour).Format(time.RFC3339)
	removed := 0
	for batch := 0; batch < 10; batch++ {
		items, err := c.db.ListImagesBefore(ctx, cutoff, 500)
		if err != nil {
			return removed, err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			if err := c.storage.Remove(item.RelPath); err != nil {
				continue
			}
			if err := c.db.DeleteImageRecord(ctx, item.ID); err == nil {
				removed++
			}
		}
		if len(items) < 500 {
			break
		}
	}
	return removed, nil
}
