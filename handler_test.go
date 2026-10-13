package slogbetterstack

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testWait = 2 * time.Second // generous: delivery runs in a goroutine

func newTestHandler(t *testing.T, status int, body string, onError func(error)) *BetterstackHandler {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	h := Option{Token: "t", Endpoint: srv.URL, OnError: onError, FlushInterval: 10 * time.Millisecond}.NewBetterstackHandler()
	t.Cleanup(func() { _ = h.Close() })

	return h
}

func TestOnError_RejectedStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusMultipleChoices} {
		errs := make(chan error, 1)
		logger := slog.New(newTestHandler(t, status, "nope", func(err error) { errs <- err }))

		logger.Info("hello")

		select {
		case err := <-errs:
			if !strings.Contains(err.Error(), "nope") {
				t.Fatalf("status %d: unexpected error %q", status, err)
			}
		case <-time.After(testWait):
			t.Fatalf("status %d: OnError not called", status)
		}
	}
}

func TestOnError_BodyIsTruncated(t *testing.T) {
	errs := make(chan error, 1)
	logger := slog.New(newTestHandler(t, http.StatusBadRequest, strings.Repeat("x", 10*maxErrorBodyBytes), func(err error) { errs <- err }))

	logger.Info("hello")

	select {
	case err := <-errs:
		if got := strings.Count(err.Error(), "x"); got != maxErrorBodyBytes {
			t.Fatalf("expected %d quoted bytes, got %d", maxErrorBodyBytes, got)
		}
	case <-time.After(testWait):
		t.Fatal("OnError not called")
	}
}

func TestOnError_NotCalledOnSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusAccepted, 299} {
		called := make(chan struct{}, 1)
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			close(done)
		}))

		h := Option{Token: "t", Endpoint: srv.URL, OnError: func(error) { called <- struct{}{} }, FlushInterval: 10 * time.Millisecond}.NewBetterstackHandler()
		slog.New(h).Info("hello")

		select {
		case <-done:
		case <-time.After(testWait):
			t.Fatalf("status %d: request not received", status)
		}
		_ = h.Close()
		srv.Close()

		select {
		case <-called:
			t.Fatalf("status %d: OnError called on success", status)
		default:
		}
	}
}

func TestOnError_NetworkAndMarshalFailure(t *testing.T) {
	errs := make(chan error, 2)

	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close() // connection refused from here on

	refused := Option{Token: "t", Endpoint: endpoint, OnError: func(err error) { errs <- err }, FlushInterval: 10 * time.Millisecond}.NewBetterstackHandler()
	defer refused.Close() //nolint:errcheck
	slog.New(refused).Info("hello")

	marshalErr := errors.New("boom")
	failing := Option{
		Token:         "t",
		Endpoint:      endpoint,
		Marshaler:     func(any) ([]byte, error) { return nil, marshalErr },
		OnError:       func(err error) { errs <- err },
		FlushInterval: 10 * time.Millisecond,
	}.NewBetterstackHandler()
	defer failing.Close() //nolint:errcheck
	slog.New(failing).Info("hello")

	sawMarshal := false
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			sawMarshal = sawMarshal || errors.Is(err, marshalErr)
		case <-time.After(testWait):
			t.Fatal("OnError not called")
		}
	}
	if !sawMarshal {
		t.Fatal("marshal error not reported")
	}
}
