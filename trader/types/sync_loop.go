package types

import (
	"nofx/logger"
	"sync"
	"time"
)

// SyncLoop owns one background task and joins it before allowing a restart.
type SyncLoop struct {
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

func (l *SyncLoop) Start(interval time.Duration, run func()) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop != nil {
		return false
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	stop, done := make(chan struct{}), make(chan struct{})
	l.stop, l.done = stop, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case <-stop:
					return
				default:
				}
				RunSyncSafely(run)
			}
		}
	}()
	return true
}

func (l *SyncLoop) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop == nil {
		return
	}
	close(l.stop)
	<-l.done
	l.stop, l.done = nil, nil
}

func RunSyncSafely(run func()) {
	defer func() {
		if p := recover(); p != nil {
			logger.Errorf("Order synchronization panic recovered: %v", p)
		}
	}()
	run()
}
