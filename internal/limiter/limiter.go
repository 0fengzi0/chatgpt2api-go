package limiter

import (
	"context"
	"errors"
	"sync"

	"github.com/0fengzi0/chatgpt2api-go/internal/config"
)

var (
	ErrQueueFull = errors.New("image request queue is full")
)

type Manager struct {
	config  *config.Store
	mu      sync.Mutex
	active  int
	waiters []chan struct{}
}

func New(cfg *config.Store) *Manager {
	return &Manager{config: cfg}
}

func (m *Manager) Acquire(ctx context.Context) (func(), error) {
	m.mu.Lock()
	cfg := m.config.Snapshot()
	maxActive := cfg.Performance.ImageWorkers
	if maxActive < 1 {
		maxActive = 1
	}
	if m.active < maxActive && len(m.waiters) == 0 {
		m.active++
		m.mu.Unlock()
		return m.releaseOnce(), nil
	}
	if len(m.waiters) >= cfg.Image.QueueSize {
		m.mu.Unlock()
		return nil, ErrQueueFull
	}
	waiter := make(chan struct{})
	m.waiters = append(m.waiters, waiter)
	m.mu.Unlock()

	select {
	case <-waiter:
		return m.releaseOnce(), nil
	case <-ctx.Done():
		m.mu.Lock()
		index := -1
		for i, item := range m.waiters {
			if item == waiter {
				index = i
				break
			}
		}
		if index >= 0 {
			m.waiters = append(m.waiters[:index], m.waiters[index+1:]...)
			m.mu.Unlock()
			return nil, ctx.Err()
		}
		// The release path granted this waiter before cancellation won the
		// select. Return the reserved slot so it cannot leak.
		m.active--
		m.grantLocked()
		m.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (m *Manager) Stats() (active, waiting int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active, len(m.waiters)
}

func (m *Manager) releaseOnce() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.active > 0 {
				m.active--
			}
			m.grantLocked()
			m.mu.Unlock()
		})
	}
}

func (m *Manager) grantLocked() {
	maxActive := m.config.Snapshot().Performance.ImageWorkers
	if maxActive < 1 {
		maxActive = 1
	}
	for m.active < maxActive && len(m.waiters) > 0 {
		next := m.waiters[0]
		m.waiters = m.waiters[1:]
		m.active++
		close(next)
	}
}
