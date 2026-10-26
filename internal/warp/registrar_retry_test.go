package warp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
)

func TestRegistrationRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, time.October, 26, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		header string
		want   time.Duration
		valid  bool
	}{
		{"seconds", " 17 ", 17 * time.Second, true},
		{"zero", "0", time.Second, true},
		{"date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{"past date", now.Add(-time.Hour).Format(http.TimeFormat), time.Second, true},
		{"large seconds", "7200", time.Hour, true},
		{"overflow seconds", strings.Repeat("9", 100), time.Hour, true},
		{"large date", now.Add(2 * time.Hour).Format(http.TimeFormat), time.Hour, true},
		{"missing", "", 0, false},
		{"invalid", "later", 0, false},
		{"negative", "-1", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			delay, valid := serverRetryDelay(test.header, now)
			if delay != test.want || valid != test.valid {
				t.Fatalf("Retry-After %q = %s,%t, want %s,%t", test.header, delay, valid, test.want, test.valid)
			}
		})
	}
}

func TestRegistrationHTTP429PausesEveryParentWithoutDialing(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("SECRET-DEVICE-CREDENTIAL"))
			}))
			defer server.Close()
			r := &registrar{base: server.URL, gap: 12 * time.Second}
			var result device
			before := time.Now()
			err := r.request(context.Background(), server.Client(), method, "/reg", "", nil, &result)
			var limited *RateLimitError
			if !errors.As(err, &limited) || limited.RetryAt.Before(before.Add(120*time.Second)) || limited.RetryAt.After(time.Now().Add(120*time.Second)) {
				t.Fatalf("429 lost its server retry deadline: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatal("rate-limit error disclosed API credentials")
			}
			var dials atomic.Int32
			parent := &transportParent{dial: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				dials.Add(1)
				return nil, errors.New("global cooldown must prevent parent dialing")
			}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			_, err = r.register(ctx, parent)
			if delay, ok := RetryAfter(fmt.Errorf("factory: %w", err)); !ok || delay < 119*time.Second {
				t.Fatalf("another parent did not receive the shared retry: %v", err)
			}
			if time.Since(start) > 200*time.Millisecond || dials.Load() != 0 || requests.Load() != 1 {
				t.Fatalf("cooldown consumed the tunnel attempt budget or sent more requests: elapsed=%s dials=%d requests=%d", time.Since(start), dials.Load(), requests.Load())
			}
		})
	}
}

func TestRegistrationRateLimitBackoffCannotMoveBackwards(t *testing.T) {
	r := new(registrar)
	for index, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		before := time.Now()
		limited := r.rateLimited("")
		if limited.RetryAt.Before(before.Add(want)) || limited.RetryAt.After(time.Now().Add(want)) {
			t.Fatalf("fallback retry %d = %s, want %s", index, time.Until(limited.RetryAt), want)
		}
	}
	longest := r.rateLimited("3600").RetryAt
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); r.rateLimited("1") }()
	}
	wg.Wait()
	if !r.next.Equal(longest) || !r.cooldownUntil.Equal(longest) {
		t.Fatal("out-of-order shorter Retry-After responses shortened the global pause")
	}
	if delay, ok := RetryAfter(&RateLimitError{RetryAt: time.Now().Add(-time.Second)}); !ok || delay != 0 {
		t.Fatal("expired rate limit was mistaken for a parent failure")
	}
	if _, ok := RetryAfter(context.DeadlineExceeded); ok {
		t.Fatal("transport timeout was mistaken for a rate limit")
	}
}

type observedRegistrationWait struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedRegistrationWait) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestRegistrationSpacingWaitReactsToConcurrentRateLimit(t *testing.T) {
	r := &registrar{next: time.Now().Add(time.Hour)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &observedRegistrationWait{Context: ctx, waiting: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- r.wait(observed) }()
	select {
	case <-observed.waiting:
	case <-time.After(time.Second):
		t.Fatal("worker did not reach its registration spacing timer")
	}
	// Acquiring the registrar mutex while another worker waits must remain
	// possible, so a 429 response can immediately wake that waiting worker.
	limited := make(chan *RateLimitError, 1)
	go func() { limited <- r.rateLimited("120") }()
	select {
	case <-limited:
	case <-time.After(time.Second):
		t.Fatal("a registration timer held the mutex needed to publish a 429 pause")
	}
	select {
	case err := <-done:
		if _, ok := RetryAfter(err); !ok {
			t.Fatalf("waiting worker lost global rate-limit state: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("429 did not wake the registration spacing wait")
	}
}

func TestRegistrationParallelWorkersPreserveSpacing(t *testing.T) {
	const workers = 5
	const gap = 25 * time.Millisecond
	r := &registrar{gap: gap}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := make(chan struct{})
	times := make(chan time.Time, workers)
	errors := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			if err := r.wait(ctx); err != nil {
				errors <- err
				return
			}
			times <- time.Now()
		}()
	}
	close(start)
	granted := make([]time.Time, 0, workers)
	for range workers {
		select {
		case at := <-times:
			granted = append(granted, at)
		case err := <-errors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	slices.SortFunc(granted, func(a, b time.Time) int { return a.Compare(b) })
	for index := 1; index < len(granted); index++ {
		if spacing := granted[index].Sub(granted[index-1]); spacing < gap-5*time.Millisecond {
			t.Fatalf("parallel workers burst registration grants: spacing=%s, want >=%s", spacing, gap)
		}
	}
}

func TestRegistrationSpacingCancellationDoesNotReserveLaterSlots(t *testing.T) {
	r := &registrar{next: time.Now().Add(time.Hour)}
	original := r.next
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("spacing wait ignored cancellation: %v", err)
	}
	if !r.next.Equal(original) {
		t.Fatal("cancelled worker reserved or reset a later registration slot")
	}
}
