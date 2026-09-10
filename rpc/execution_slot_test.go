package rpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSlotReleaseFreesCapacity(t *testing.T) {
	t.Parallel()

	pool := NewExecutionPool(1, 0, "test", false)

	released := make(chan struct{})
	unblock := make(chan struct{})

	pool.SubmitWithSlot(context.Background(), func(slot *Slot) error {
		slot.Release()
		close(released)
		<-unblock

		return nil
	})
	<-released
	defer close(unblock)

	ran := make(chan struct{})
	pool.Submit(context.Background(), func() error {
		close(ran)

		return nil
	})

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("second task never got a slot while the first was parked")
	}
}

func TestSlotReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	pool := NewExecutionPool(1, 0, "test", false)

	done := make(chan struct{})
	pool.SubmitWithSlot(context.Background(), func(slot *Slot) error {
		slot.Release()
		slot.Release()
		close(done)

		return nil
	})
	<-done

	// A second release would have drained a slot nobody held, letting the pool
	// admit two tasks at size 1.
	if got := pool.inFlight.Load(); got != 0 {
		t.Fatalf("inFlight = %d, want 0", got)
	}

	sem := *pool.sem.Load()
	if got := len(sem); got != 0 {
		t.Fatalf("semaphore holds %d slots, want 0", got)
	}
}

func TestSlotAcquireIsBestEffort(t *testing.T) {
	t.Parallel()

	pool := NewExecutionPool(1, 0, "test", false)

	held := make(chan bool, 1)
	pool.SubmitWithSlot(context.Background(), func(slot *Slot) error {
		slot.Release()

		// Saturate the pool behind the slot's back, so reacquiring can only
		// succeed by blocking — which it must not do.
		sem := *pool.sem.Load()
		sem <- struct{}{}

		defer func() { <-sem }()

		slot.Acquire()
		held <- slot.held

		return nil
	})

	select {
	case got := <-held:
		if got {
			t.Fatal("Acquire reported a hold it never got")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire blocked on a saturated pool")
	}
}

func TestSlotAcquireAfterChangeSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		newSize  int
		wantHeld bool
	}{
		{name: "resized", newSize: 4, wantHeld: true},
		{name: "unbounded", newSize: 0, wantHeld: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pool := NewExecutionPool(1, 0, "test", false)

			held := make(chan bool, 1)
			pool.SubmitWithSlot(context.Background(), func(slot *Slot) error {
				slot.Release()
				pool.ChangeSize(tt.newSize)
				slot.Acquire()
				held <- slot.held

				return nil
			})

			if got := <-held; got != tt.wantHeld {
				t.Fatalf("held = %v, want %v", got, tt.wantHeld)
			}
		})
	}
}

func TestSubmitWithSlotUnboundedPoolHasNoSlot(t *testing.T) {
	t.Parallel()

	pool := NewExecutionPool(0, 0, "test", false)

	slots := make(chan *Slot, 1)
	pool.SubmitWithSlot(context.Background(), func(slot *Slot) error {
		slots <- slot

		return nil
	})

	if slot := <-slots; slot != nil {
		t.Fatalf("slot = %v, want nil on the unbounded fast path", slot)
	}
}

// TestLongCallDoesNotStarveTransport is the regression this whole mechanism
// exists for: a handler that parks waiting on something other than work must
// not hold the transport's only execution slot while it does.
//
// It has to go over HTTP. DialInProc serves the connection through a client
// handler with its own 100-slot pool, so an in-process call never touches the
// server's execution pool at all.
func TestLongCallDoesNotStarveTransport(t *testing.T) {
	t.Parallel()

	svc, url, stop := newParkingServer(t, false)
	defer stop()

	parkDone := park(t, url, svc)
	defer parkDone()

	if err := callQuick(url, 2*time.Second); err != nil {
		t.Fatalf("quick call blocked behind the parked one: %v", err)
	}
}

// TestHeldSlotStarvesTransport pins the other side of the same behaviour: a
// handler that keeps its slot while parked does block the transport, which is
// what SendRawTransactionSync used to do.
func TestHeldSlotStarvesTransport(t *testing.T) {
	t.Parallel()

	svc, url, stop := newParkingServer(t, true)
	defer stop()

	parkDone := park(t, url, svc)
	defer parkDone()

	if err := callQuick(url, 500*time.Millisecond); err == nil {
		t.Fatal("quick call succeeded while the only slot was held")
	}
}

func newParkingServer(t *testing.T, keepSlot bool) (*parkingService, string, func()) {
	t.Helper()

	server := NewServer("test", 1, 0)

	svc := &parkingService{
		parked:  make(chan struct{}),
		unblock: make(chan struct{}),
		keep:    keepSlot,
	}
	if err := server.RegisterName("test", svc); err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(server)

	return svc, httpServer.URL, func() {
		httpServer.Close()
		server.Stop()
	}
}

// park starts a call that occupies the handler and waits until it is in the
// park, returning a function that lets it finish.
func park(t *testing.T, url string, svc *parkingService) func() {
	t.Helper()

	go func() {
		_, _ = http.Post(url, "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"test_park","params":[]}`))
	}()

	select {
	case <-svc.parked:
	case <-time.After(5 * time.Second):
		t.Fatal("park handler never ran")
	}

	return func() { close(svc.unblock) }
}

func callQuick(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"test_quick","params":[]}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if !strings.Contains(string(body), `"quick"`) {
		return fmt.Errorf("unexpected response %q", body)
	}

	return nil
}

type parkingService struct {
	parked  chan struct{}
	unblock chan struct{}
	keep    bool // hold the slot across the park instead of releasing it
}

func (s *parkingService) Park(ctx context.Context) (string, error) {
	if !s.keep {
		slot := ExecutionSlotFromContext(ctx)
		slot.Release()

		defer slot.Acquire()
	}

	close(s.parked)
	<-s.unblock

	return "parked", nil
}

func (s *parkingService) Quick() (string, error) {
	return "quick", nil
}

// TestSubmitFallsBackWhenSaturated covers the two escape hatches that keep a
// full pool from blocking a submitter forever: an elapsed pool timeout, and a
// cancelled caller context. In both cases fn still has to run, because the
// caller's WaitGroup is counting on it.
func TestSubmitFallsBackWhenSaturated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		timeout time.Duration
		ctx     func() (context.Context, context.CancelFunc)
	}{
		{
			name:    "pool timeout elapses",
			timeout: 10 * time.Millisecond,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.Background(), func() {}
			},
		},
		{
			name:    "caller gives up",
			timeout: 0,
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, func() {}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pool := NewExecutionPool(1, tt.timeout, "test", false)

			// Take the only slot and keep it.
			sem := *pool.sem.Load()
			sem <- struct{}{}

			defer func() { <-sem }()

			ctx, cancel := tt.ctx()
			defer cancel()

			ran := make(chan struct{})
			pool.Submit(ctx, func() error {
				close(ran)

				return nil
			})

			select {
			case <-ran:
			case <-time.After(2 * time.Second):
				t.Fatal("fn never ran on a saturated pool")
			}
		})
	}
}

func TestNilSlotMethodsAreNoOps(t *testing.T) {
	t.Parallel()

	var slot *Slot

	// A handler on the unbounded fast path gets no slot and must not have to
	// check for one.
	slot.Release()
	slot.Acquire()
}

func TestAcquireWhileHeldTakesNothing(t *testing.T) {
	t.Parallel()

	pool := NewExecutionPool(2, 0, "test", false)

	type state struct {
		held     bool
		semLen   int
		inFlight int64
	}

	got := make(chan state, 1)
	pool.SubmitWithSlot(context.Background(), func(slot *Slot) error {
		// Already holding one; acquiring again must not take a second.
		slot.Acquire()

		got <- state{
			held:     slot.held,
			semLen:   len(*pool.sem.Load()),
			inFlight: pool.inFlight.Load(),
		}

		return nil
	})

	s := <-got
	if !s.held {
		t.Fatal("slot stopped reporting a hold it still has")
	}
	if s.semLen != 1 {
		t.Fatalf("semaphore holds %d slots, want 1", s.semLen)
	}
	if s.inFlight != 1 {
		t.Fatalf("inFlight = %d, want 1", s.inFlight)
	}
}

func TestReacquireRestoresInFlight(t *testing.T) {
	t.Parallel()

	pool := NewExecutionPool(2, 0, "test", false)

	type sample struct{ released, reacquired int64 }

	got := make(chan sample, 1)
	pool.SubmitWithSlot(context.Background(), func(slot *Slot) error {
		slot.Release()
		released := pool.inFlight.Load()

		slot.Acquire()
		got <- sample{released: released, reacquired: pool.inFlight.Load()}

		return nil
	})

	s := <-got
	if s.released != 0 {
		t.Fatalf("inFlight = %d while released, want 0", s.released)
	}
	if s.reacquired != 1 {
		t.Fatalf("inFlight = %d after reacquiring, want 1", s.reacquired)
	}
}

func TestFastPathHandlerSeesNoSlotOnContext(t *testing.T) {
	t.Parallel()

	if slot := ExecutionSlotFromContext(context.Background()); slot != nil {
		t.Fatalf("slot = %v, want nil when no pool published one", slot)
	}

	if got := withExecutionSlot(context.Background(), nil); ExecutionSlotFromContext(got) != nil {
		t.Fatal("a nil slot was published on the context")
	}
}
