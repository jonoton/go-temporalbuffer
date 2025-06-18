package temporalbuffer

import (
	"math/rand"
	"sync"
	"testing"
	"time"
)

// --- Mock DataItem ---

// mockItem is a mock implementation of DataItem, Cleanable, and Referenceable
// for testing purposes.
type mockItem struct {
	id          int
	created     time.Time
	cleanedUp   bool
	refCount    int
	cleanupLock sync.Mutex
	refLock     sync.Mutex
}

func (m *mockItem) CreatedTime() time.Time {
	return m.created
}

func (m *mockItem) Cleanup() {
	m.cleanupLock.Lock()
	defer m.cleanupLock.Unlock()
	m.cleanedUp = true
}

func (m *mockItem) Ref() DataItem {
	m.refLock.Lock()
	defer m.refLock.Unlock()
	m.refCount++
	// Return a new mock item to simulate creating a new reference.
	return &mockItem{id: m.id, created: m.created}
}

func (m *mockItem) hasBeenCleanedUp() bool {
	m.cleanupLock.Lock()
	defer m.cleanupLock.Unlock()
	return m.cleanedUp
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

// --- Tests ---

func TestNewBufferWithOptions(t *testing.T) {
	t.Run("Default options", func(t *testing.T) {
		b := New(5)
		defer b.Close()
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

	item1 := &mockItem{id: 1, created: time.Now()}
	b.Add(item1)

	// Allow the buffer's manager goroutine time to process the operation.
	time.Sleep(10 * time.Millisecond)

	retrieved, ok := b.TryGetOldest()
	assertTrue(t, ok, "TryGetOldest should succeed on non-empty buffer")
	assertEqual(t, retrieved.(*mockItem).id, 1, "Retrieved item ID mismatch")

	_, ok = b.TryGetOldest()
	assertTrue(t, !ok, "TryGetOldest should fail on empty buffer")
}

func TestGetAll(t *testing.T) {
	b := New(5)
	defer b.Close()

	items := []DataItem{
		&mockItem{id: 1, created: time.Now()},
		&mockItem{id: 2, created: time.Now().Add(1 * time.Second)},
	}
	b.AddAll(items)

	// Allow the buffer's manager goroutine time to process the operation.
	time.Sleep(10 * time.Millisecond)

	all := b.GetAll()
	assertEqual(t, len(all), 5, "GetAll should return all items (including filled)")

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

func TestDropStrategy_DropClosest(t *testing.T) {
	b := New(3)
	defer b.Close()

	items := []*mockItem{
		{id: 1, created: time.Unix(100, 0)},
		{id: 2, created: time.Unix(200, 0)},
		{id: 3, created: time.Unix(201, 0)},
		{id: 4, created: time.Unix(210, 0)},
	}

	b.AddAll([]DataItem{items[0], items[1], items[2], items[3]})
	// Allow the buffer's manager goroutine time to process the operation.
	time.Sleep(20 * time.Millisecond)

	result := b.GetAll()
	assertEqual(t, len(result), 3, "Buffer should have 3 items after dropping")

	ids := make(map[int]bool)
	for _, item := range result {
		ids[item.(*mockItem).id] = true
	}

	if _, found := ids[2]; found {
		t.Fatal("Item 2 should have been dropped")
	}
	assertTrue(t, items[1].hasBeenCleanedUp(), "Dropped item should be cleaned up")
}

func TestDropStrategy_DropOldest(t *testing.T) {
	b := New(2, WithDropStrategy(DropOldest))
	defer b.Close()

	items := []*mockItem{
		{id: 1, created: time.Unix(100, 0)}, // Will be dropped
		{id: 2, created: time.Unix(200, 0)},
		{id: 3, created: time.Unix(300, 0)},
	}

	b.AddAll([]DataItem{items[0], items[1], items[2]})
	// Allow the buffer's manager goroutine time to process the operation.
	time.Sleep(20 * time.Millisecond)

	result := b.GetAll()
	assertEqual(t, len(result), 2, "Buffer should have 2 items")
	assertEqual(t, result[0].(*mockItem).id, 2, "First item should be id 2")
	assertEqual(t, result[1].(*mockItem).id, 3, "Second item should be id 3")
	assertTrue(t, items[0].hasBeenCleanedUp(), "Dropped item 1 should be cleaned up")
}

func TestFillStrategy(t *testing.T) {
	t.Run("FillLargestGap", func(t *testing.T) {
		b := New(5, WithFillStrategy(FillLargestGap))
		defer b.Close()

		items := []*mockItem{
			{id: 1, created: time.Unix(100, 0)},
			{id: 2, created: time.Unix(200, 0)},
		}
		b.AddAll([]DataItem{items[0], items[1]})
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(20 * time.Millisecond)

		result := b.GetAll()
		assertEqual(t, len(result), 5, "Buffer should be filled to capacity")

		idCounts := make(map[int]int)
		for _, item := range result {
			idCounts[item.(*mockItem).id]++
		}
		assertEqual(t, idCounts[1], 4, "Item 1 should have 4 slots (1 original + 3 filled)")
		assertEqual(t, idCounts[2], 1, "Item 2 should have 1 slot")
	})

	t.Run("FillLargestGap_SingleItem", func(t *testing.T) {
		b := New(5, WithFillStrategy(FillLargestGap))
		defer b.Close()

		item := &mockItem{id: 1, created: time.Unix(100, 0)}
		b.Add(item)
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(20 * time.Millisecond)

		result := b.GetAll()
		assertEqual(t, len(result), 5, "Buffer should be filled to capacity with one item")
		for _, resItem := range result {
			assertEqual(t, resItem.(*mockItem).id, 1, "All items should be a copy of the single item")
		}
	})

	t.Run("ResampleTimeline", func(t *testing.T) {
		b := New(10, WithFillStrategy(ResampleTimeline))
		defer b.Close()

		items := []*mockItem{
			{id: 1, created: time.Now()},
			{id: 2, created: time.Now().Add(1 * time.Second)},
			{id: 3, created: time.Now().Add(2 * time.Second)},
		}
		b.AddAll([]DataItem{items[0], items[1], items[2]})
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(20 * time.Millisecond)

		result := b.GetAll()
		assertEqual(t, len(result), 10, "Buffer should be filled to capacity")

		idCounts := make(map[int]int)
		for _, item := range result {
			idCounts[item.(*mockItem).id]++
		}
		assertEqual(t, idCounts[1], 4, "Item 1 should have 4 slots")
		assertEqual(t, idCounts[2], 3, "Item 2 should have 3 slots")
		assertEqual(t, idCounts[3], 3, "Item 3 should have 3 slots")
	})

	t.Run("PadWithNewest", func(t *testing.T) {
		b := New(5, WithFillStrategy(PadWithNewest))
		defer b.Close()
		b.Add(&mockItem{id: 1, created: time.Now()})
		b.Add(&mockItem{id: 2, created: time.Now().Add(1 * time.Second)})
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(20 * time.Millisecond)

		result := b.GetAll()
		assertEqual(t, len(result), 5, "Buffer should be filled to capacity")
		assertEqual(t, result[0].(*mockItem).id, 1, "First item should be original")
		assertEqual(t, result[1].(*mockItem).id, 2, "Second item should be original")
		assertEqual(t, result[2].(*mockItem).id, 2, "Third item should be padded duplicate")
	})

	t.Run("NoFill", func(t *testing.T) {
		b := New(5, WithFillStrategy(NoFill))
		defer b.Close()
		b.Add(&mockItem{id: 1, created: time.Now()})
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(10 * time.Millisecond)

		assertEqual(t, b.Len(), 1, "Buffer should not be filled")
	})
}

func TestReadContinuity(t *testing.T) {
	t.Run("Enabled by default", func(t *testing.T) {
		b := New(5)
		defer b.Close()

		_, ok := b.TryGetOldest()
		assertTrue(t, !ok, "TryGetOldest on a never-used buffer should fail")

		item1 := &mockItem{id: 1, created: time.Now()}
		b.Add(item1)
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(20 * time.Millisecond)

		retrieved1, ok := b.TryGetOldest()
		assertTrue(t, ok, "First TryGetOldest should succeed")
		assertEqual(t, retrieved1.(*mockItem).id, 1, "Retrieved item ID should be 1")

		_ = b.GetAll()
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(10 * time.Millisecond)

		continuityItem, ok := b.TryGetOldest()
		assertTrue(t, ok, "TryGetOldest on an empty buffer should succeed with continuity")
		assertNotNil(t, continuityItem, "Continuity item should not be nil")
		assertEqual(t, continuityItem.(*mockItem).id, 1, "Continuity item should have the ID of the last-read item")
	})

	t.Run("Disabled", func(t *testing.T) {
		b := New(5, WithReadContinuity(false), WithFillStrategy(NoFill))
		defer b.Close()

		item1 := &mockItem{id: 1, created: time.Now()}
		b.Add(item1)
		// Allow the buffer's manager goroutine time to process the operation.
		time.Sleep(10 * time.Millisecond)
		_, _ = b.TryGetOldest() // Drain the only item

		_, ok := b.TryGetOldest()
		assertTrue(t, !ok, "TryGetOldest on an empty buffer should fail when continuity is disabled")
	})

	t.Run("Streaming channel continuity", func(t *testing.T) {
		b := New(2, WithFillStrategy(NoFill))
		defer b.Close()
		stream := b.GetOldestChan()

		b.Add(&mockItem{id: 1, created: time.Now()})

		item1 := <-stream
		assertEqual(t, item1.(*mockItem).id, 1, "First stream item should be real")

		continuityItem1 := <-stream
		assertEqual(t, continuityItem1.(*mockItem).id, 1, "Stream should provide continuity item")
	})
}

func TestGetOldest_Blocking(t *testing.T) {
	b := New(5, WithReadContinuity(false), WithFillStrategy(NoFill))
	defer b.Close()

	done := make(chan DataItem)
	go func() {
		// This will block until an item is added.
		item := b.GetOldest()
		done <- item
	}()

	// Verify the goroutine is blocked before proceeding.
	time.Sleep(20 * time.Millisecond)

	item1 := &mockItem{id: 100, created: time.Now()}
	b.Add(item1)

	select {
	case retrieved := <-done:
		assertNotNil(t, retrieved, "Retrieved item should not be nil")
		assertEqual(t, retrieved.(*mockItem).id, 100, "Blocking GetOldest returned wrong item")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Test timed out waiting for blocking GetOldest to return")
	}
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
				b.Add(&mockItem{id: id, created: time.Now()})
				time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)
			}
		}(i)
	}

	// Single consumer
	receivedCount := 0
	stream := b.GetOldestChan()
	done := make(chan struct{})
	totalItems := numProducers * itemsPerProducer

	go func() {
		for range stream {
			receivedCount++
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
	case <-time.After(2 * time.Second):
		t.Fatalf("Test timed out. Expected to consume %d items, but only got %d", totalItems, receivedCount)
	}
}

func TestLenAndCap(t *testing.T) {
	b := New(10)
	defer b.Close()

	assertEqual(t, b.Cap(), 10, "Cap() should return the configured size")
	assertEqual(t, b.Len(), 0, "Len() on new buffer should be 0")

	b.Add(&mockItem{id: 1, created: time.Now()})
	// Allow the buffer's manager goroutine time to process the operation.
	time.Sleep(10 * time.Millisecond)
	assertEqual(t, b.Len(), 10, "Len() after add with resampling should be 10")
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
