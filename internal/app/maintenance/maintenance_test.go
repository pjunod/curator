package maintenance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestDrainRetainsActualChildrenAndRejectsReleasedToken(t *testing.T) {
	c := New()
	parent, release, err := c.TryEnter(context.Background(), "copy and close")
	if err != nil {
		t.Fatal(err)
	}
	owner := strings.Repeat("ab", 32)
	status, err := c.Start(owner)
	if err != nil || status.Phase != "draining" {
		t.Fatalf("%+v %v", status, err)
	}
	if _, _, err = c.TryEnter(context.Background(), "new copy"); !errors.Is(err, ports.ErrMaintenance) {
		t.Fatalf("new root admitted: %v", err)
	}
	child, done, err := c.TryEnter(parent, "result transaction")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	if c.Status().Active != 1 {
		t.Fatal(c.Status())
	}
	if _, _, err = c.TryEnter(parent, "late producer"); !errors.Is(err, ports.ErrMaintenance) {
		t.Fatal("released parent started work")
	}
	if child.Err() != nil {
		t.Fatal("drain cancelled admitted work")
	}
	done()
	if c.Status().Phase != "drained" {
		t.Fatal(c.Status())
	}
	if _, _, err = c.TryEnter(child, "after zero"); !errors.Is(err, ports.ErrMaintenance) {
		t.Fatal("new child after zero")
	}
	if _, err = c.Abort(owner, status.Generation); err != nil {
		t.Fatal(err)
	}
	_, finished, err := c.TryEnter(context.Background(), "resumed")
	if err != nil {
		t.Fatal(err)
	}
	finished()
	next, err := c.Start(owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Abort(owner, status.Generation); !errors.Is(err, ErrOwner) {
		t.Fatal("stale generation reopened new drain")
	}
	c.Close()
	if _, err = c.Abort(owner, next.Generation); !errors.Is(err, ErrOwner) {
		t.Fatal("terminal reopened")
	}
}

func TestAdmissionStartBarrierHasNoUnregisteredWriter(t *testing.T) {
	for round := 0; round < 100; round++ {
		c := New()
		start := make(chan struct{})
		var wg sync.WaitGroup
		entered := make(chan func(), 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, release, err := c.TryEnter(context.Background(), "writer")
			if err == nil {
				entered <- release
			}
		}()
		close(start)
		_, err := c.Start(strings.Repeat("01", 32))
		if err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		select {
		case done := <-entered:
			if c.Status().Active != 1 {
				t.Fatal("admitted writer not visible")
			}
			done()
		default:
			if c.Status().Active != 0 {
				t.Fatal(c.Status())
			}
		}
		if c.Status().Phase != "drained" {
			t.Fatal(c.Status())
		}
	}
}

func TestWaitingProducerResumesOrCancelsWithoutOwningDrain(t *testing.T) {
	c := New()
	owner := strings.Repeat("02", 32)
	status, err := c.Start(owner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, release, e := c.Enter(ctx, "periodic producer")
		if release != nil {
			release()
		}
		result <- e
	}()
	if c.Status().Active != 0 {
		t.Fatal("idle waiter counted as actual writer")
	}
	cancel()
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not cancel")
	}
	if _, err = c.Start(strings.Repeat("03", 32)); !errors.Is(err, ErrOwner) {
		t.Fatal("different owner took drain")
	}
	again, err := c.Start(owner)
	if err != nil || again.Generation != status.Generation {
		t.Fatal("lost response was not idempotent")
	}
	if _, err = c.Abort(owner, status.Generation); err != nil {
		t.Fatal(err)
	}
	_, done, err := c.Enter(context.Background(), "resumed producer")
	if err != nil {
		t.Fatal(err)
	}
	done()
}
