package orchestrator

import "sync"

// Pass-through can expose the raw terminal before the transformed consumer
// closes. Wait only for required usage capture, never optional observation I/O.
type accountingCaptureBarrier struct {
	done chan struct{}
	once sync.Once
	err  error
}

func newAccountingCaptureBarrier() *accountingCaptureBarrier {
	return &accountingCaptureBarrier{done: make(chan struct{})}
}
func (b *accountingCaptureBarrier) finish(err error) {
	if b != nil {
		b.once.Do(func() { b.err = err; close(b.done) })
	}
}
