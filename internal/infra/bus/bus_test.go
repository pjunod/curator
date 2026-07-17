package bus

import (
	"testing"
	"time"
)

type testEvent struct{ n int }

func (testEvent) EventType() string { return "test.event" }

type otherEvent struct{}

func (otherEvent) EventType() string { return "other.event" }

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		panic("unreachable")
	}
}

func TestFanOut(t *testing.T) {
	b := New(nil)
	defer b.Close()

	ch1, cancel1 := b.SubscribeAll(4)
	ch2, cancel2 := b.SubscribeAll(4)
	defer cancel1()
	defer cancel2()

	b.Publish(testEvent{n: 1})

	if e := recv(t, ch1); e.(testEvent).n != 1 {
		t.Errorf("sub1 got %v", e)
	}
	if e := recv(t, ch2); e.(testEvent).n != 1 {
		t.Errorf("sub2 got %v", e)
	}
}

func TestUnsubscribeStopsDeliveryAndClosesChannel(t *testing.T) {
	b := New(nil)
	defer b.Close()

	ch, cancel := b.SubscribeAll(4)
	b.Publish(testEvent{n: 1})
	recv(t, ch)

	cancel()
	b.Publish(testEvent{n: 2})

	// Channel must be closed and drained — no event 2.
	select {
	case e, ok := <-ch:
		if ok {
			t.Fatalf("received %v after unsubscribe", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after unsubscribe")
	}
}

func TestTypedSubscribeFilters(t *testing.T) {
	b := New(nil)
	defer b.Close()

	ch, cancel := Subscribe[testEvent](b, 4)
	defer cancel()

	b.Publish(otherEvent{})
	b.Publish(testEvent{n: 7})

	if e := recv(t, ch); e.n != 7 {
		t.Errorf("got %+v, want n=7", e)
	}
}

func TestPublishNeverBlocksOnFullSubscriber(t *testing.T) {
	b := New(nil)
	defer b.Close()

	_, cancel := b.SubscribeAll(1) // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(testEvent{n: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
}

func TestCloseIsIdempotentAndClosesSubscribers(t *testing.T) {
	b := New(nil)
	ch, _ := b.SubscribeAll(1)
	b.Close()
	b.Close()
	if _, ok := <-ch; ok {
		t.Error("subscriber channel should be closed after bus Close")
	}
	// Publishing after close must not panic.
	b.Publish(testEvent{})
	// Subscribing after close returns a closed channel.
	ch2, cancel := b.SubscribeAll(1)
	cancel()
	if _, ok := <-ch2; ok {
		t.Error("subscribe after close should return a closed channel")
	}
}
