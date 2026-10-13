package slogbetterstack

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testBackoff = time.Millisecond

// flakyServer answers with the given statuses in order, then 202.
func flakyServer(statuses ...int) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(calls.Add(1)) - 1
		if n < len(statuses) {
			w.WriteHeader(statuses[n])
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))

	return srv, &calls
}

func logOnceAndClose(t *testing.T, o Option) []error {
	t.Helper()

	var errs []error
	o.Token = "t"
	o.RetryBackoff = testBackoff
	o.OnError = func(err error) { errs = append(errs, err) }

	h := o.NewBetterstackHandler()
	slog.New(h).Info("hello")
	_ = h.Close() // Close waits for the worker, so errs is safe to read afterwards

	return errs
}

func TestRetry_RecoversFromRetryableStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		srv, calls := flakyServer(status, status)

		errs := logOnceAndClose(t, Option{Endpoint: srv.URL})
		srv.Close()

		if len(errs) != 0 || calls.Load() != 3 {
			t.Fatalf("status %d: expected 3 calls and no error, got %d calls, errors %v", status, calls.Load(), errs)
		}
	}
}

func TestRetry_DoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge} {
		srv, calls := flakyServer(status, status, status)

		errs := logOnceAndClose(t, Option{Endpoint: srv.URL})
		srv.Close()

		if len(errs) != 1 || calls.Load() != 1 {
			t.Fatalf("status %d: expected 1 call and 1 error, got %d calls, errors %v", status, calls.Load(), errs)
		}
	}
}

func TestRetry_GivesUpAfterMaxRetries(t *testing.T) {
	srv, calls := flakyServer(500, 500, 500, 500, 500, 500)
	defer srv.Close()

	errs := logOnceAndClose(t, Option{Endpoint: srv.URL, MaxRetries: 2})

	if calls.Load() != 3 {
		t.Fatalf("expected 1 attempt + 2 retries, got %d calls", calls.Load())
	}
	if len(errs) != 1 {
		t.Fatalf("expected the last error to be reported once, got %v", errs)
	}
}

func TestRetry_NegativeMaxRetriesDisablesRetries(t *testing.T) {
	srv, calls := flakyServer(500, 500)
	defer srv.Close()

	errs := logOnceAndClose(t, Option{Endpoint: srv.URL, MaxRetries: -1})

	if calls.Load() != 1 || len(errs) != 1 {
		t.Fatalf("expected 1 call and 1 error, got %d calls, errors %v", calls.Load(), errs)
	}
}

func TestRetry_NetworkErrorIsRetried(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close()

	var attempts atomic.Int32
	errs := logOnceAndClose(t, Option{
		Endpoint:   endpoint,
		MaxRetries: 2,
		Marshaler: func(v any) ([]byte, error) {
			attempts.Add(1) // marshaling runs once per attempt
			return []byte("[]"), nil
		},
	})

	if attempts.Load() != 3 || len(errs) != 1 {
		t.Fatalf("expected 3 attempts and 1 error, got %d attempts, errors %v", attempts.Load(), errs)
	}
}

func TestRetry_MarshalErrorIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	boom := errors.New("boom")

	errs := logOnceAndClose(t, Option{
		Endpoint: "http://127.0.0.1:1",
		Marshaler: func(any) ([]byte, error) {
			attempts.Add(1)
			return nil, boom
		},
	})

	if attempts.Load() != 1 || len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Fatalf("expected 1 attempt and the marshal error, got %d attempts, errors %v", attempts.Load(), errs)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&statusError{code: 199}, false},
		{&statusError{code: 400}, false},
		{&statusError{code: 428}, false},
		{&statusError{code: 429}, true},
		{&statusError{code: 499}, false},
		{&statusError{code: 500}, true},
		{&statusError{code: 599}, true},
		{&permanentError{errors.New("x")}, false},
		{errors.New("connection refused"), true},
	}

	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
