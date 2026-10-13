package slogbetterstack

import (
	"crypto/rand"
	"errors"
	"math/big"
	"sync"
	"time"
)

const (
	defaultBatchSize     = 100
	defaultFlushInterval = time.Second
	defaultQueueSize     = 1000
	defaultMaxRetries    = 3
	defaultRetryBackoff  = 500 * time.Millisecond

	// maxRetryBackoff bounds the doubling delay (before jitter, which adds up to 50%), so a high MaxRetries does not stall Close for minutes.
	maxRetryBackoff = 30 * time.Second
)

// ErrQueueFull is reported through OnError when a record is dropped because
// the delivery queue is saturated. Dropping keeps logging non-blocking.
var ErrQueueFull = errors.New("betterstack log queue full, record dropped")

// ErrHandlerClosed is reported through OnError when a record is logged after Close.
var ErrHandlerClosed = errors.New("betterstack handler closed, record dropped")

// dispatcher owns the delivery queue and the single worker goroutine. It is
// shared by a handler and every handler derived from it with WithAttrs or WithGroup.
type dispatcher struct {
	option Option
	queue  chan map[string]any
	done   chan struct{}

	// mu orders enqueue against Close: closing the queue while a sender is
	// between the closed check and the channel send would panic.
	mu     sync.RWMutex
	closed bool
}

func newDispatcher(option Option) *dispatcher {
	d := &dispatcher{
		option: option,
		queue:  make(chan map[string]any, option.QueueSize),
		done:   make(chan struct{}),
	}

	go d.run()

	return d
}

// enqueue never blocks.
func (d *dispatcher) enqueue(payload map[string]any) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed {
		d.option.OnError(ErrHandlerClosed)
		return
	}

	select {
	case d.queue <- payload:
	default:
		d.option.OnError(ErrQueueFull)
	}
}

// close stops accepting records, delivers what is still queued, then returns.
func (d *dispatcher) close() {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()

	<-d.done
}

// run sends a batch when it is full, when the flush interval elapses, or when the queue is closed.
func (d *dispatcher) run() {
	defer close(d.done)

	ticker := time.NewTicker(d.option.FlushInterval)
	defer ticker.Stop()

	batch := make([]map[string]any, 0, d.option.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}

		err := d.sendWithRetry(batch)
		if err != nil {
			d.option.OnError(err)
		}

		// a fresh slice: Marshaler implementations may retain the payload
		batch = make([]map[string]any, 0, d.option.BatchSize)
	}

	for {
		select {
		case payload, ok := <-d.queue:
			if !ok {
				flush()
				return
			}

			batch = append(batch, payload)
			if len(batch) >= d.option.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// sendWithRetry retries retryable failures with jittered exponential backoff and
// returns the last error. A negative MaxRetries disables retries.
func (d *dispatcher) sendWithRetry(batch []map[string]any) error {
	backoff := d.option.RetryBackoff

	var err error
	for attempt := 0; ; attempt++ {
		err = send(d.option.Endpoint, d.option.Token, d.option.Timeout, d.option.Marshaler, batch)
		if err == nil || !retryable(err) || attempt >= d.option.MaxRetries {
			return err
		}

		time.Sleep(jitter(backoff))

		backoff *= 2
		if backoff > maxRetryBackoff {
			backoff = maxRetryBackoff
		}
	}
}

// jitter returns a random duration in [d, d+d/2]. Without it, handlers that failed
// together (a server outage hitting every replica) would retry in lockstep.
// It only ever lengthens the wait, so the backoff stays a lower bound.
//
// crypto/rand is not needed for security here. It keeps the static analysis
// gate (weak PRNG rule) green and costs nothing next to a network retry.
func jitter(d time.Duration) time.Duration {
	spread := d / 2
	if spread <= 0 {
		return d
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(spread)+1))
	if err != nil {
		// no randomness available: waiting the plain backoff is still correct
		return d
	}

	return d + time.Duration(n.Int64())
}
