package temporalbuffer

import (
	"math/rand"
	"sync"
	"testing"
	"time"
)

// --- Mock DataItem ---

// mockItem is a mock implementation of ManagedItem for testing purposes.
// It allows us to track reference counts to verify memory management.
type mockItem struct {
	id       int
	created  time.Time
	refCount int
	refLock  sync.Mutex
}

// newMockItem creates a new item with an initial reference count of 1,
// simulating the caller's initial ownership of the item.
func newMockItem(id int, t time.Time) *mockItem {
	return &mockItem{id: id, created: t, refCount: 1}
}

func (m *mockItem) CreatedTime() time.Time { return m.created }

// Ref increments the reference count, simulating a new reference being taken.
// It returns a pointer to the same item, as Ref() creates a new logical
// reference to the same underlying data.
func (m *mockItem) Ref() DataItem {
	m.refLock.Lock()
	defer m.refLock.Unlock()
	m.refCount++
	return m
}

// Cleanup decrements the reference count.
func (m *mockItem) Cleanup() {
	m.refLock.Lock()
	defer m.refLock.Unlock()
	m.refCount--
}

func (m *mockItem) getRefCount() int {
	m.refLock.Lock()
	defer m.refLock.Unlock()
	return m.refCount
}

// --- Helper Functions ---

func assertEqual(t *testing.T, a, b int, msg string) {
	t.Helper()
	if a != b {
		t.Fatalf("%s: expected %d, got %d", msg, b, a)
	}
}

func assertTrue(t *testing.T, v bool, msg string) {
	t.Helper()
	if !v {
		t.Fatalf("%s: expected true, got false", msg)
	}
}

func assertNotNil(t *testing.T, v interface{}, msg string) {
	t.Helper()
	if v == nil {
		t.Fatalf("%s: expected not nil, got nil", msg)
	}
}

// waitForCondition waits until a condition is met or a timeout occurs.
func waitForCondition(t *testing.T, condition func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- Tests ---

func TestNewBufferWithOptions(t *testing.T) {
	t.Run("Default options", func(t *testing.T) {
		b := New(5)
		defer b.Close()
		// This test inspects internal state, which is generally discouraged, but useful
		// for verifying the constructor logic.
		assertEqual(t, int(b.opts.fillStrategy), int(ResampleTimeline), "Default fillStrategy should be ResampleTimeline")
		assertTrue(t, b.opts.readContinuity, "Default readContinuity should be true")
		assertEqual(t, int(b.opts.dropStrategy), int(DropClosest), "Default dropStrategy should be DropClosest")
	})

	t.Run("With custom options", func(t *testing.T) {
		b := New(5, WithFillStrategy(PadWithNewest), WithDropStrategy(DropOldest), WithReadContinuity(false))
		defer b.Close()
		assertEqual(t, int(b.opts.fillStrategy), int(PadWithNewest), "WithFillStrategy should be set")
		assertTrue(t, !b.opts.readContinuity, "WithReadContinuity(false) should be set")
		assertEqual(t, int(b.opts.dropStrategy), int(DropOldest), "WithDropStrategy(DropOldest) should be set")
	})
}

func TestAddAndGet(t *testing.T) {
	b := New(5, WithFillStrategy(NoFill), WithReadContinuity(false))
	defer b.Close()

	item1 := newMockItem(1, time.Now())
	b.Add(item1)

	waitForCondition(t, func() bool { return b.Len() == 1 }, 50*time.Millisecond)

	retrieved, ok := b.TryGetOldest()
	assertTrue(t, ok, "TryGetOldest should succeed on non-empty buffer")
	assertEqual(t, retrieved.(*mockItem).id, 1, "Retrieved item ID mismatch")

	_, ok = b.TryGetOldest()
	assertTrue(t, !ok, "TryGetOldest should fail on empty buffer")

	// The test now owns the retrieved item
	retrieved.(ManagedItem).Cleanup()
}

func TestGetAll(t *testing.T) {
	b := New(5)
	defer b.Close()

	items := []DataItem{
		newMockItem(1, time.Now()),
		newMockItem(2, time.Now().Add(1*time.Second)),
	}
	b.AddAll(items)

	waitForCondition(t, func() bool { return b.Len() == 5 }, 50*time.Millisecond)

	all := b.GetAll()
	assertEqual(t, len(all), 5, "GetAll should return all items")
	// The test now owns the retrieved items
	cleanupItems(all)

	allAfterClear := b.GetAll()
	if allAfterClear != nil {
		t.Fatalf("GetAll on a cleared buffer should return nil, got %d items", len(allAfterClear))
	}
}

func TestGetAll_NewBuffer(t *testing.T) {
	b := New(5)
	defer b.Close()

	all := b.GetAll()
	if all != nil {
		t.Fatalf("GetAll on a new, empty buffer should return nil, got %d items", len(all))
	}
}

func TestDropStrategy(t *testing.T) {
	t.Run("DropClosest", func(t *testing.T) {
		b := New(3, WithFillStrategy(NoFill))
		defer b.Close()

		items := []*mockItem{
			newMockItem(1, time.Now()),
			newMockItem(2, time.Now().Add(1*time.Second)), // To be dropped
			newMockItem(3, time.Now().Add(2*time.Second)),
			newMockItem(4, time.Now().Add(12*time.Second)),
		}

		b.AddAll([]DataItem{items[0], items[1], items[2], items[3]})
		waitForCondition(t, func() bool { return b.Len() == 3 }, 50*time.Millisecond)

		b.Close() // Close the buffer to ensure all internal cleanup is done.

		// The buffer should have cleaned up the item completely.
		assertEqual(t, items[1].getRefCount(), 0, "Dropped item should be fully cleaned up")
	})

	t.Run("DropOldest", func(t *testing.T) {
		b := New(2, WithDropStrategy(DropOldest), WithFillStrategy(NoFill))
		defer b.Close()

		items := []*mockItem{
			newMockItem(1, time.Now()), // To be dropped
			newMockItem(2, time.Now().Add(1*time.Second)),
			newMockItem(3, time.Now().Add(2*time.Second)),
		}

		b.AddAll([]DataItem{items[0], items[1], items[2]})
		waitForCondition(t, func() bool { return b.Len() == 2 }, 50*time.Millisecond)
		b.Close()

		// The buffer should have cleaned up the item completely.
		assertEqual(t, items[0].getRefCount(), 0, "Dropped item should be fully cleaned up")
	})
}

func TestFillStrategy(t *testing.T) {
	t.Run("ResampleTimeline", func(t *testing.T) {
		b := New(10, WithFillStrategy(ResampleTimeline))
		defer b.Close()

		items := []*mockItem{
			newMockItem(1, time.Now()),
			newMockItem(2, time.Now().Add(1*time.Second)),
			newMockItem(3, time.Now().Add(2*time.Second)),
		}
		b.AddAll([]DataItem{items[0], items[1], items[2]})
		waitForCondition(t, func() bool { return b.Len() == 10 }, 50*time.Millisecond)

		// Check counts while buffer is active
		// Real(1) + Display(4,3,3)
		assertEqual(t, items[0].getRefCount(), 1+4, "Item 1 ref count is wrong")
		assertEqual(t, items[1].getRefCount(), 1+3, "Item 2 ref count is wrong")
		assertEqual(t, items[2].getRefCount(), 1+3, "Item 3 ref count is wrong")
	})

	t.Run("PadWithNewest", func(t *testing.T) {
		b := New(5, WithFillStrategy(PadWithNewest))
		defer b.Close()

		items := []*mockItem{
			newMockItem(1, time.Now()),
			newMockItem(2, time.Now().Add(1*time.Second)),
		}
		b.AddAll([]DataItem{items[0], items[1]})
		waitForCondition(t, func() bool { return b.Len() == 5 }, 50*time.Millisecond)

		all := b.GetAll()
		assertEqual(t, all[0].(*mockItem).id, 1, "PadWithNewest should have item 1 at index 0")
		assertEqual(t, all[1].(*mockItem).id, 2, "PadWithNewest should have item 2 at index 1")
		assertEqual(t, all[2].(*mockItem).id, 2, "PadWithNewest should have item 2 at index 2")
		cleanupItems(all)
	})

	t.Run("FillLargestGap", func(t *testing.T) {
		b := New(5, WithFillStrategy(FillLargestGap))
		defer b.Close()

		items := []*mockItem{
			newMockItem(1, time.Unix(100, 0)),
			newMockItem(2, time.Unix(200, 0)),
		}
		b.AddAll([]DataItem{items[0], items[1]})
		waitForCondition(t, func() bool { return b.Len() == 5 }, 50*time.Millisecond)

		all := b.GetAll()
		idCounts := make(map[int]int)
		for _, item := range all {
			idCounts[item.(*mockItem).id]++
		}
		assertEqual(t, idCounts[1], 4, "Item 1 should have 4 slots (1 original, 3 filled)")
		assertEqual(t, idCounts[2], 1, "Item 2 should have 1 slot")
		cleanupItems(all)
	})
}

// TestMemoryManagement verifies the reference counting logic based on the final ownership model.
func TestMemoryManagement(t *testing.T) {
	t.Run("Add transfers ownership", func(t *testing.T) {
		b := New(5, WithFillStrategy(NoFill))
		item1 := newMockItem(1, time.Now())

		b.Add(item1)
		waitForCondition(t, func() bool { return b.Len() == 1 }, 50*time.Millisecond)

		b.Close() // Close buffer to release its internal refs
		waitForCondition(t, func() bool { return item1.getRefCount() == 0 }, 50*time.Millisecond)
		assertEqual(t, item1.getRefCount(), 0, "Ref count should be 0 after buffer closes")
	})

	t.Run("Get consumes display and real item", func(t *testing.T) {
		b := New(1, WithFillStrategy(NoFill))
		item1 := newMockItem(1, time.Now())
		b.Add(item1)
		waitForCondition(t, func() bool { return b.Len() == 1 }, 50*time.Millisecond)

		gottenItem, _ := b.TryGetOldest()
		waitForCondition(t, func() bool { return b.Len() == 0 }, 50*time.Millisecond)

		b.Close()
		waitForCondition(t, func() bool { return item1.getRefCount() == 1 }, 50*time.Millisecond)
		// After close, only the user's reference from `gottenItem` should remain.
		assertEqual(t, item1.getRefCount(), 1, "Ref count should be 1 after close")

		gottenItem.(ManagedItem).Cleanup()
		assertEqual(t, item1.getRefCount(), 0, "Ref count should be 0 after final cleanup")
	})

	t.Run("GetAll transfers ownership and cleans internal refs", func(t *testing.T) {
		b := New(2, WithFillStrategy(NoFill))
		item1 := newMockItem(1, time.Now())
		b.Add(item1)
		waitForCondition(t, func() bool { return b.Len() == 1 }, 50*time.Millisecond)

		gottenItems := b.GetAll()
		waitForCondition(t, func() bool { return b.Len() == 0 }, 50*time.Millisecond)

		b.Close() // Close should have no effect on refs already transferred.

		// After GetAll, the user owns the display references. The buffer has cleaned its real refs.
		assertEqual(t, item1.getRefCount(), 1, "Ref count after GetAll is wrong")

		cleanupItems(gottenItems)
		assertEqual(t, item1.getRefCount(), 0, "Item should be fully cleaned up")
	})

	t.Run("lastReadItem lifecycle", func(t *testing.T) {
		b := New(2, WithFillStrategy(NoFill))
		item1 := newMockItem(1, time.Now())
		item2 := newMockItem(2, time.Now())

		b.Add(item1)
		b.Add(item2)
		waitForCondition(t, func() bool { return b.Len() == 2 }, 50*time.Millisecond)

		gottenItem1, _ := b.TryGetOldest()
		waitForCondition(t, func() bool { return b.Len() == 1 }, 50*time.Millisecond)

		gottenItem2, _ := b.TryGetOldest()
		waitForCondition(t, func() bool { return b.Len() == 0 }, 50*time.Millisecond)

		b.Close()
		waitForCondition(t, func() bool { return item1.getRefCount() == 1 && item2.getRefCount() == 1 }, 50*time.Millisecond)
		// After close, user owns gottenItem1 and gottenItem2.
		assertEqual(t, item1.getRefCount(), 1, "Ref count for item1 should be 1")
		assertEqual(t, item2.getRefCount(), 1, "Ref count for item2 should be 1")

		gottenItem1.(ManagedItem).Cleanup()
		gottenItem2.(ManagedItem).Cleanup()
		assertEqual(t, item1.getRefCount(), 0, "Item 1 should be fully cleaned")
		assertEqual(t, item2.getRefCount(), 0, "Item 2 should be fully cleaned")
	})
}

func TestGetOldest_Blocking(t *testing.T) {
	b := New(5, WithReadContinuity(false))
	defer b.Close()

	done := make(chan bool)
	go func() {
		item := b.GetOldest()
		assertNotNil(t, item, "Blocking GetOldest should return a valid item")
		assertEqual(t, item.(*mockItem).id, 100, "Blocking GetOldest returned wrong item")
		item.(ManagedItem).Cleanup()
		done <- true
	}()

	// Give the goroutine time to block on GetOldest
	time.Sleep(20 * time.Millisecond)

	item1 := newMockItem(100, time.Now())
	b.Add(item1)

	select {
	case <-done:
		// success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Test timed out waiting for blocking GetOldest to return")
	}
}

func TestCap(t *testing.T) {
	b := New(99)
	defer b.Close()
	assertEqual(t, b.Cap(), 99, "Cap() should return the configured size")
}

func TestConcurrentOperations(t *testing.T) {
	b := New(50)
	defer b.Close()

	numProducers := 5
	itemsPerProducer := 20
	var wg sync.WaitGroup
	wg.Add(numProducers)

	// Producers
	for i := 0; i < numProducers; i++ {
		go func(producerID int) {
			defer wg.Done()
			for j := 0; j < itemsPerProducer; j++ {
				id := (producerID * itemsPerProducer) + j
				b.Add(newMockItem(id, time.Now()))
				time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)
			}
		}(i)
	}

	// Single consumer
	receivedCount := 0
	stream := b.GetOldestChan()
	done := make(chan struct{})
	totalItems := numProducers * itemsPerProducer
	var receivedItems []DataItem

	go func() {
		for item := range stream {
			receivedCount++
			receivedItems = append(receivedItems, item)
			if receivedCount == totalItems {
				close(done)
				return
			}
		}
	}()

	wg.Wait() // Wait for all producers to finish adding

	select {
	case <-done:
		// All items were consumed before close, success
		cleanupItems(receivedItems)
	case <-time.After(2 * time.Second):
		cleanupItems(receivedItems)
		t.Fatalf("Test timed out. Expected to consume %d items, but only got %d", totalItems, receivedCount)
	}
}

func TestClose_Idempotency(t *testing.T) {
	b := New(5)
	b.Close()
	// Calling Close() on an already closed buffer should not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("The code panicked on second close: %v", r)
			}
		}()
		b.Close()
	}()
}
