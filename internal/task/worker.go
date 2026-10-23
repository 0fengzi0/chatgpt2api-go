package task

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
	"github.com/0fengzi0/chatgpt2api-go/internal/image"
	"github.com/0fengzi0/chatgpt2api-go/internal/provider"
	"github.com/0fengzi0/chatgpt2api-go/internal/store"
)

type Worker struct {
	store    *store.Store
	config   *config.Store
	storage  *image.Storage
	provider provider.Provider
	logger   *slog.Logger
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func NewWorker(s *store.Store, cfg *config.Store, storage *image.Storage, p provider.Provider, logger *slog.Logger) *Worker {
	return &Worker{store: s, config: cfg, storage: storage, provider: p, logger: logger}
}

func (w *Worker) Start(ctx context.Context) {
	cfg := w.config.Snapshot()
	workers := cfg.Performance.ImageWorkers
	if workers < 1 {
		workers = 1
	}
	ctx, w.cancel = context.WithCancel(ctx)
	for i := 0; i < workers; i++ {
		w.wg.Add(1)
		go w.loop(ctx, i+1)
	}
}

func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

func (w *Worker) loop(ctx context.Context, number int) {
	defer w.wg.Done()
	w.logger.Info("image worker started", "worker", number)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("image worker stopped", "worker", number)
			return
		case <-time.After(500 * time.Millisecond):
		}
		job, ok, err := w.store.ClaimJob(ctx, 30*time.Minute)
		if err != nil {
			w.logger.Warn("claim image job", "error", err)
			continue
		}
		if !ok {
			continue
		}
		w.process(ctx, job)
	}
}

func (w *Worker) process(parent context.Context, job store.Job) {
	cfg := w.config.Snapshot()
	timeout := time.Duration(cfg.Image.PollTimeoutSeconds+30) * time.Second
	if timeout < time.Minute {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	_ = w.store.UpdateJobProgress(ctx, job.ID, "starting")
	var inputImages [][]byte
	inputs, err := w.store.ListJobInputs(ctx, job.ID)
	if err != nil {
		_ = w.store.FinishJob(ctx, job.ID, nil, "load job inputs: "+err.Error())
		return
	}
	for _, input := range inputs {
		f, _, err := w.storage.OpenInput(input.RelPath)
		if err != nil {
			_ = w.store.FinishJob(ctx, job.ID, nil, "open job input: "+err.Error())
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(f, cfg.Image.MaxImageBytes))
		f.Close()
		if readErr != nil {
			_ = w.store.FinishJob(ctx, job.ID, nil, "read job input: "+readErr.Error())
			return
		}
		inputImages = append(inputImages, data)
	}
	result, err := w.provider.Generate(ctx, provider.Request{
		Prompt:  job.Prompt,
		Model:   job.Model,
		Size:    job.Size,
		Quality: job.Quality,
		Images:  inputImages,
		Progress: func(step string) {
			_ = w.store.UpdateJobProgress(ctx, job.ID, step)
		},
	})
	if err != nil {
		if job.Attempts < job.MaxAttempts && parent.Err() == nil {
			delay := time.Duration(job.Attempts) * 5 * time.Second
			stateCtx, cancelState := finalContext(parent)
			_ = w.store.RetryJob(stateCtx, job.ID, delay, err.Error())
			cancelState()
			w.logger.Warn("image job retry", "job", job.ID, "attempt", job.Attempts, "error", err)
			return
		}
		stateCtx, cancelState := finalContext(parent)
		_ = w.store.FinishJob(stateCtx, job.ID, nil, err.Error())
		cancelState()
		w.logger.Warn("image job failed", "job", job.ID, "error", err)
		return
	}
	stateCtx, cancelState := finalContext(parent)
	defer cancelState()
	saved := make([]store.Image, 0, len(result.Images))
	for _, data := range result.Images {
		img, err := w.storage.Save(stateCtx, job.Owner, job.ID, data, result.MIME)
		if err != nil {
			_ = w.store.FinishJob(stateCtx, job.ID, nil, "save image: "+err.Error())
			return
		}
		if err := w.store.AddImage(stateCtx, img); err != nil {
			_ = w.store.FinishJob(stateCtx, job.ID, nil, "save image metadata: "+err.Error())
			return
		}
		saved = append(saved, img)
	}
	if len(saved) == 0 {
		_ = w.store.FinishJob(stateCtx, job.ID, nil, "provider returned no images")
		return
	}
	_ = w.store.FinishJob(stateCtx, job.ID, map[string]any{"images": saved}, "")
	w.logger.Info("image job succeeded", "job", job.ID, "images", len(saved))
}

func finalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}
