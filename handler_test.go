package slogbetterstack

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testWait = 2 * time.Second // generous: delivery runs in a goroutine

func newTestHandler(t *testing.T, status int, body string, onError func(error)) slog.Handler {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return Option{Token: "t", Endpoint: srv.URL, OnError: onError}.NewBetterstackHandler()
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

		logger := slog.New(Option{Token: "t", Endpoint: srv.URL, OnError: func(error) { called <- struct{}{} }}.NewBetterstackHandler())
		logger.Info("hello")

		select {
		case <-done:
		case <-time.After(testWait):
			t.Fatalf("status %d: request not received", status)
		}
		time.Sleep(50 * time.Millisecond) // let the sender goroutine return
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

	slog.New(Option{Token: "t", Endpoint: endpoint, OnError: func(err error) { errs <- err }}.NewBetterstackHandler()).Info("hello")

	marshalErr := errors.New("boom")
	slog.New(Option{
		Token:     "t",
		Endpoint:  endpoint,
		Marshaler: func(any) ([]byte, error) { return nil, marshalErr },
		OnError:   func(err error) { errs <- err },
	}.NewBetterstackHandler()).Info("hello")

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

func TestOnError_DefaultWritesNothing(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	stderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = stderr }()

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	h := Option{Token: "t", Endpoint: srv.URL}.NewBetterstackHandler()
	slog.New(h).Info("hello")
	time.Sleep(100 * time.Millisecond) // the failing send runs in a goroutine

	_ = w.Close()
	out, _ := io.ReadAll(r)
	if len(out) != 0 {
		t.Fatalf("expected no output by default, got %q", out)
	}
}
