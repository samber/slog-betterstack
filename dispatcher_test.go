package slogbetterstack

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// collector records the size of every batch the server receives.
type collector struct {
	mu      sync.Mutex
	batches []int
	records int
}

func (c *collector) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		var records []map[string]any
		if err := json.Unmarshal(raw, &records); err != nil {
			t.Errorf("body is not a JSON array: %v", err)
		}

		c.mu.Lock()
		c.batches = append(c.batches, len(records))
		c.records += len(records)
		c.mu.Unlock()

		w.WriteHeader(http.StatusAccepted)
	}
}

func (c *collector) snapshot() (batches []int, records int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]int(nil), c.batches...), c.records
}

func TestClose_DeliversQueuedRecords(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler(t))
	defer srv.Close()

	// the interval is far longer than the test: only Close can flush
	h := Option{Token: "t", Endpoint: srv.URL, FlushInterval: time.Hour}.NewBetterstackHandler()
	logger := slog.New(h)

	for i := 0; i < 7; i++ {
		logger.Info("hello", "i", i)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	if _, records := c.snapshot(); records != 7 {
		t.Fatalf("expected 7 records delivered by Close, got %d", records)
	}
}

func TestBatching_SplitsAtBatchSize(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler(t))
	defer srv.Close()

	h := Option{Token: "t", Endpoint: srv.URL, BatchSize: 3, FlushInterval: time.Hour}.NewBetterstackHandler()
	logger := slog.New(h)

	for i := 0; i < 7; i++ {
		logger.Info("hello")
	}
	_ = h.Close()

	batches, records := c.snapshot()
	if records != 7 || len(batches) != 3 || batches[0] != 3 || batches[1] != 3 || batches[2] != 1 {
		t.Fatalf("expected batches [3 3 1], got %v", batches)
	}
}

func TestBatching_FlushesOnInterval(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler(t))
	defer srv.Close()

	h := Option{Token: "t", Endpoint: srv.URL, FlushInterval: 20 * time.Millisecond}.NewBetterstackHandler()
	defer h.Close() //nolint:errcheck

	slog.New(h).Info("hello")

	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if _, records := c.snapshot(); records == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("partial batch not flushed by the interval")
}

func TestClose_IsIdempotentAndCoversDerivedHandlers(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler(t))
	defer srv.Close()

	h := Option{Token: "t", Endpoint: srv.URL, FlushInterval: time.Hour}.NewBetterstackHandler()
	slog.New(h).With("a", 1).WithGroup("g").Info("derived")

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	if _, records := c.snapshot(); records != 1 {
		t.Fatalf("expected the derived handler's record to be delivered, got %d", records)
	}
}

func TestHandle_AfterCloseReportsError(t *testing.T) {
	errs := make(chan error, 1)
	h := Option{Token: "t", Endpoint: "http://127.0.0.1:1", OnError: func(err error) { errs <- err }}.NewBetterstackHandler()
	_ = h.Close()

	slog.New(h).Info("too late")

	select {
	case err := <-errs:
		if err != ErrHandlerClosed {
			t.Fatalf("unexpected error: %v", err)
		}
	default:
		t.Fatal("record after Close not reported")
	}
}

func TestHandle_QueueFullDropsWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the worker inside its first send
	}))
	defer srv.Close()

	errs := make(chan error, 100)
	h := Option{
		Token: "t", Endpoint: srv.URL, BatchSize: 1, QueueSize: 1, FlushInterval: time.Hour,
		OnError: func(err error) { errs <- err },
	}.NewBetterstackHandler()
	logger := slog.New(h)

	// 1st record is taken by the stuck worker, 2nd fills the queue, the rest must drop.
	for i := 0; i < 20; i++ {
		logger.Info("burst")
	}

	select {
	case err := <-errs:
		if err != ErrQueueFull {
			t.Fatalf("unexpected error: %v", err)
		}
	default:
		t.Fatal("no drop reported although the queue is full")
	}

	close(release)
	_ = h.Close()
}

func TestHandle_ConcurrentWithClose(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c.handler(t))
	defer srv.Close()

	h := Option{Token: "t", Endpoint: srv.URL, OnError: func(error) {}}.NewBetterstackHandler()
	logger := slog.New(h)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				logger.Info("race")
			}
		}()
	}

	_ = h.Close() // must not panic with "send on closed channel"
	wg.Wait()
}
