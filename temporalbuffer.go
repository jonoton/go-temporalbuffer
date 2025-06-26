package temporalbuffer

import (
	"sort"
	"sync"
	"time"
)

// --- Enums and Options ---

// DropStrategy defines the method for dropping items when the buffer is full.
type DropStrategy int

const (
	// DropClosest finds the two items closest in time and drops the older one.
	DropClosest DropStrategy = iota
	// DropOldest simply drops the oldest item in the buffer.
	DropOldest
)

// FillStrategy defines the method for filling the buffer when it is under capacity.
type FillStrategy int

const (
	// NoFill will not add any items to the buffer.
	NoFill FillStrategy = iota
	// PadWithNewest adds duplicates of the newest item until the buffer is full.
	PadWithNewest
	// ResampleTimeline rebuilds the buffer to be an evenly distributed timeline of the items it contains.
	// This provides the smoothest possible statistical output. This is the default strategy.
	ResampleTimeline
	// FillLargestGap iteratively finds the largest time gap between items and inserts a duplicate
	// of the earlier item, preserving the original timeline as closely as possible.
	FillLargestGap
)

// options holds the configuration for a Buffer.
type options struct {
	fillStrategy   FillStrategy
	readContinuity bool
	dropStrategy   DropStrategy
}

// Option is a function that configures a Buffer's options.
type Option func(*options)

// WithFillStrategy sets the strategy for filling the buffer when it is under capacity.
// The default is ResampleTimeline.
func WithFillStrategy(strategy FillStrategy) Option {
	return func(o *options) {
		o.fillStrategy = strategy
	}
}

// WithReadContinuity enables or disables providing the last-read item on a read from an empty buffer.
// Enabled by default.
func WithReadContinuity(enabled bool) Option {
	return func(o *options) {
		o.readContinuity = enabled
	}
}

// WithDropStrategy sets the strategy for dropping items when the buffer is full.
// The default is DropClosest.
func WithDropStrategy(strategy DropStrategy) Option {
	return func(o *options) {
		o.dropStrategy = strategy
	}
}

// --- Interfaces ---

// DataItem is the constraint that all items stored in the buffer must satisfy.
type DataItem interface {
	CreatedTime() time.Time
}

// ManagedItem is an optional interface for items that require reference counting
// and cleanup logic. The generic type T ensures that Ref() returns the concrete type.
type ManagedItem[T DataItem] interface {
	DataItem  // Embed the constraint interface.
	Ref() T   // Should increment a reference count and return a new reference.
	Cleanup() // Should decrement a reference count and free resources if the count is zero.
}

// --- Internal Types ---

type tryGetResult[T DataItem] struct {
	item T
	ok   bool
}

// bufferState holds the internal, mutable state of the buffer.
type bufferState[T DataItem] struct {
	realItems        []T
	displayItems     []T
	displayMap       []int // Maps displayItems back to the index of their source realItem
	lastReadItem     T
	streamOutputChan chan<- T
	opts             options
	size             int
}

// --- Buffer Implementation ---

// Buffer is a thread-safe, fixed-size buffer for time-stamped data.
// It is generic and can hold any type that satisfies the DataItem interface.
type Buffer[T DataItem] struct {
	size               int
	opts               options
	state              *bufferState[T] // The internal state, confined to the run() goroutine.
	done               chan struct{}
	addChan            chan T
	addAllChan         chan []T
	getOldestChan      chan chan T
	tryGetOldestChan   chan chan tryGetResult[T]
	getAllChan         chan chan []T
	registerStreamChan chan chan T
	lenChan            chan chan int
	quitChan           chan struct{}
	closeOnce          sync.Once
}

// New creates a new Buffer for a specific type T with the given size and optional configurations.
// The type T must satisfy the DataItem interface.
func New[T DataItem](size int, opts ...Option) *Buffer[T] {
	cfg := options{
		fillStrategy:   ResampleTimeline,
		readContinuity: true,
		dropStrategy:   DropClosest,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	b := &Buffer[T]{
		size: size,
		opts: cfg,
		state: &bufferState[T]{
			realItems: make([]T, 0, size),
			opts:      cfg,
			size:      size,
		},
		addChan:            make(chan T),
		addAllChan:         make(chan []T),
		getOldestChan:      make(chan chan T),
		tryGetOldestChan:   make(chan chan tryGetResult[T]),
		getAllChan:         make(chan chan []T),
		registerStreamChan: make(chan chan T),
		lenChan:            make(chan chan int),
		quitChan:           make(chan struct{}),
		done:               make(chan struct{}),
	}
	go b.run()
	return b
}

// run is the manager goroutine that owns and manages the buffer state.
func (b *Buffer[T]) run() {
	defer close(b.done)
	state := b.state
	var zeroValue T // The zero value for type T (e.g., nil for pointers)

	for {
		var oldestChan chan chan T
		var streamChan chan<- T
		var streamValue T

		lastReadItemCreatedReference := false
		canServe := len(state.displayItems) > 0 || (state.opts.readContinuity && any(state.lastReadItem) != any(zeroValue))
		if canServe {
			oldestChan = b.getOldestChan
			streamChan = state.streamOutputChan
			if len(state.displayItems) > 0 {
				streamValue = state.displayItems[0]
			} else {
				streamValue = createItemRef(state.lastReadItem)
				lastReadItemCreatedReference = true
			}
		}
		cleanupUnusedReference := func() {
			if lastReadItemCreatedReference {
				cleanupItem(streamValue)
			}
		}

		if canServe {
			select {
			case item := <-b.addChan:
				cleanupUnusedReference()
				state.handleAdd(item)
			case newItems := <-b.addAllChan:
				cleanupUnusedReference()
				state.handleAddAll(newItems)
			case respChan := <-oldestChan:
				cleanupUnusedReference()
				state.handleGetOldest(respChan)
			case respChan := <-b.tryGetOldestChan:
				cleanupUnusedReference()
				state.handleTryGetOldest(respChan)
			case respChan := <-b.getAllChan:
				cleanupUnusedReference()
				state.handleGetAll(respChan)
			case reqChan := <-b.registerStreamChan:
				cleanupUnusedReference()
				state.streamOutputChan = reqChan
			case streamChan <- streamValue:
				state.handleStreamSend()
			case respChan := <-b.lenChan:
				cleanupUnusedReference()
				respChan <- len(state.displayItems)
			case <-b.quitChan:
				cleanupUnusedReference()
				cleanupItems(state.displayItems)
				cleanupItems(state.realItems)
				cleanupItem(state.lastReadItem)
				if state.streamOutputChan != nil {
					close(state.streamOutputChan)
				}
				return
			}
		} else {
			// This select block handles the case where the buffer is completely empty
			// and has no last-read item to provide for continuity.
			select {
			case item := <-b.addChan:
				state.handleAdd(item)
			case newItems := <-b.addAllChan:
				state.handleAddAll(newItems)
			case respChan := <-b.getOldestChan:
				item := <-b.addChan
				state.handleAdd(item)
				state.handleGetOldest(respChan)
			case respChan := <-b.tryGetOldestChan:
				respChan <- tryGetResult[T]{item: zeroValue, ok: false}
			case respChan := <-b.getAllChan:
				respChan <- nil
			case reqChan := <-b.registerStreamChan:
				state.streamOutputChan = reqChan
			case respChan := <-b.lenChan:
				respChan <- 0
			case <-b.quitChan:
				cleanupItem(state.lastReadItem)
				if state.streamOutputChan != nil {
					close(state.streamOutputChan)
				}
				return
			}
		}
	}
}

// --- State Handlers ---

// rebuildDisplayBuffer recalculates the user-facing slice based on the real items
// and the configured drop/fill strategies.
func (s *bufferState[T]) rebuildDisplayBuffer() {
	// 1. Clean up old display items before creating new ones.
	cleanupItems(s.displayItems)
	// 2. Determine the new set of real items after dropping.
	s.realItems = applyDropStrategy(s.realItems, s.size, s.opts.dropStrategy)
	// 3. Generate the new display items and their map from the real items.
	s.displayItems, s.displayMap = applyFillStrategy(s.realItems, s.size, s.opts.fillStrategy)
}

// setLastReadItem correctly manages the lifecycle of the continuity item.
func (s *bufferState[T]) setLastReadItem(item T) {
	cleanupItem(s.lastReadItem)
	s.lastReadItem = createItemRef(item)
}

// handleAdd takes ownership of the incoming item and rebuilds the display.
func (s *bufferState[T]) handleAdd(item T) {
	s.realItems = append(s.realItems, item)
	s.rebuildDisplayBuffer()
}

// handleAddAll takes ownership of all incoming items and rebuilds the display.
func (s *bufferState[T]) handleAddAll(items []T) {
	s.realItems = append(s.realItems, items...)
	s.rebuildDisplayBuffer()
}

// consumeAndCheckRealItem determines if a real item is no longer represented
// in the display and can be safely cleaned up and removed.
func (s *bufferState[T]) consumeAndCheckRealItem(consumedIndex int) {
	if consumedIndex < 0 || consumedIndex >= len(s.realItems) {
		return
	}
	isStillPresent := false
	for _, mappedIndex := range s.displayMap {
		if mappedIndex == consumedIndex {
			isStillPresent = true
			break
		}
	}
	if isStillPresent {
		return
	}

	// No display items point to this real item anymore, so clean it up.
	cleanupItem(s.realItems[consumedIndex])
	s.realItems = append(s.realItems[:consumedIndex], s.realItems[consumedIndex+1:]...)
	// The removal has shifted indices, so the map must be adjusted.
	for i, mappedIndex := range s.displayMap {
		if mappedIndex > consumedIndex {
			s.displayMap[i]--
		}
	}
}

// handleGetOldest handles a blocking get request.
func (s *bufferState[T]) handleGetOldest(respChan chan T) {
	if len(s.displayItems) > 0 {
		itemToReturn := s.displayItems[0]
		consumedRealItemIndex := s.displayMap[0]

		s.displayItems = s.displayItems[1:]
		s.displayMap = s.displayMap[1:]

		s.consumeAndCheckRealItem(consumedRealItemIndex)
		s.setLastReadItem(itemToReturn)

		respChan <- itemToReturn
	} else {
		// Provide a continuity item, which is a new reference.
		respChan <- createItemRef(s.lastReadItem)
	}
}

// handleTryGetOldest handles a non-blocking get request.
func (s *bufferState[T]) handleTryGetOldest(respChan chan tryGetResult[T]) {
	if len(s.displayItems) > 0 {
		itemToReturn := s.displayItems[0]
		consumedRealItemIndex := s.displayMap[0]

		s.displayItems = s.displayItems[1:]
		s.displayMap = s.displayMap[1:]

		s.consumeAndCheckRealItem(consumedRealItemIndex)
		s.setLastReadItem(itemToReturn)

		respChan <- tryGetResult[T]{item: itemToReturn, ok: true}
	} else if s.opts.readContinuity && any(s.lastReadItem) != any(*new(T)) {
		respChan <- tryGetResult[T]{item: createItemRef(s.lastReadItem), ok: true}
	} else {
		respChan <- tryGetResult[T]{item: *new(T), ok: false}
	}
}

// handleGetAll transfers ownership of all display items and cleans up internal state.
func (s *bufferState[T]) handleGetAll(respChan chan []T) {
	if len(s.displayItems) == 0 {
		respChan <- nil
	} else {
		respChan <- s.displayItems
	}
	// Ownership of all display items is transferred. The buffer is no longer
	// responsible for them. However, it is discarding its internal list of
	// real items, so it must clean up those references.
	cleanupItems(s.realItems)
	s.realItems = make([]T, 0, s.size)
	s.displayItems = nil
	s.displayMap = nil
}

// handleStreamSend consumes the oldest item for the streaming channel.
func (s *bufferState[T]) handleStreamSend() {
	if len(s.displayItems) > 0 {
		itemSent := s.displayItems[0]
		consumedRealItemIndex := s.displayMap[0]

		// Do not cleanup itemSent, ownership is transferred to the stream consumer.
		s.setLastReadItem(itemSent)
		s.displayItems = s.displayItems[1:]
		s.displayMap = s.displayMap[1:]

		s.consumeAndCheckRealItem(consumedRealItemIndex)
	}
}

// --- Pure Functions ---

// cleanupItem handles resource cleanup for a single item.
func cleanupItem[T DataItem](item T) {
	var zeroValue T
	if any(item) == any(zeroValue) {
		return
	}
	if managed, ok := any(item).(ManagedItem[T]); ok {
		managed.Cleanup()
	}
}

// cleanupItems handles resource cleanup for a slice of items.
func cleanupItems[T DataItem](items []T) {
	for _, item := range items {
		cleanupItem(item)
	}
}

// createItemRef is a pure helper to safely call the Ref() method.
func createItemRef[T DataItem](item T) T {
	var zeroValue T
	if any(item) == any(zeroValue) {
		return zeroValue
	}
	if managed, ok := any(item).(ManagedItem[T]); ok {
		return managed.Ref()
	}
	return item
}

// applyDropStrategy is a pure function that sorts and drops items if over capacity.
func applyDropStrategy[T DataItem](items []T, size int, strategy DropStrategy) []T {
	if len(items) <= size {
		return items
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedTime().Before(items[j].CreatedTime()) })
	switch strategy {
	case DropOldest:
		droppedCount := len(items) - size
		dropped := items[:droppedCount]
		items = items[droppedCount:]
		cleanupItems(dropped)
	case DropClosest:
		for len(items) > size {
			if len(items) <= 1 {
				break
			}
			var minDiff time.Duration = -1
			dropIndex := -1
			for i := 0; i < len(items)-1; i++ {
				diff := items[i+1].CreatedTime().Sub(items[i].CreatedTime())
				if dropIndex == -1 || diff < minDiff {
					minDiff = diff
					dropIndex = i
				}
			}
			if dropIndex != -1 {
				itemToDrop := items[dropIndex]
				items = append(items[:dropIndex], items[dropIndex+1:]...)
				cleanupItem(itemToDrop)
			} else {
				break
			}
		}
	}
	return items
}

// applyFillStrategy generates a new display slice and its corresponding map.
func applyFillStrategy[T DataItem](items []T, size int, strategy FillStrategy) ([]T, []int) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) >= size {
		display := make([]T, len(items))
		displayMap := make([]int, len(items))
		for i, item := range items {
			display[i] = createItemRef(item)
			displayMap[i] = i
		}
		return display, displayMap
	}
	switch strategy {
	case ResampleTimeline:
		return fillResampleTimeline(items, size)
	case PadWithNewest:
		return fillPadWithNewest(items, size)
	case FillLargestGap:
		return fillLargestGap(items, size)
	case NoFill: // Fallthrough
	}
	// For NoFill, we still need to create references for the items that are present.
	display := make([]T, len(items))
	displayMap := make([]int, len(items))
	for i, item := range items {
		display[i] = createItemRef(item)
		displayMap[i] = i
	}
	return display, displayMap
}

// fillResampleTimeline generates a display slice using proportional representation.
func fillResampleTimeline[T DataItem](items []T, size int) ([]T, []int) {
	resampled := make([]T, 0, size)
	resampledMap := make([]int, 0, size)
	numItems := len(items)
	baseCount := size / numItems
	extraCount := size % numItems
	for i, item := range items {
		count := baseCount
		if i < extraCount {
			count++
		}
		for j := 0; j < count; j++ {
			resampled = append(resampled, createItemRef(item))
			resampledMap = append(resampledMap, i)
		}
	}
	return resampled, resampledMap
}

// fillPadWithNewest generates a display slice by padding with the newest item.
func fillPadWithNewest[T DataItem](items []T, size int) ([]T, []int) {
	filledItems := make([]T, 0, size)
	filledMap := make([]int, 0, size)
	for i, item := range items {
		filledItems = append(filledItems, createItemRef(item))
		filledMap = append(filledMap, i)
	}
	newestItem := items[len(items)-1]
	newestItemIndex := len(items) - 1
	for i := len(items); i < size; i++ {
		filledItems = append(filledItems, createItemRef(newestItem))
		filledMap = append(filledMap, newestItemIndex)
	}
	return filledItems, filledMap
}

// fillLargestGap generates a display slice by filling the largest time gaps.
func fillLargestGap[T DataItem](items []T, size int) ([]T, []int) {
	filledItems := make([]T, 0, size)
	filledMap := make([]int, 0, size)
	for i, item := range items {
		filledItems = append(filledItems, createItemRef(item))
		filledMap = append(filledMap, i)
	}
	for len(filledItems) < size {
		if len(filledItems) < 2 {
			if len(filledItems) == 1 {
				filledItems = append(filledItems, createItemRef(filledItems[0]))
				filledMap = append(filledMap, filledMap[0])
				continue
			}
			break
		}
		var maxDiff time.Duration = -1
		insertIndex := -1
		for i := 0; i < len(filledItems)-1; i++ {
			diff := filledItems[i+1].CreatedTime().Sub(filledItems[i].CreatedTime())
			if diff > maxDiff {
				maxDiff = diff
				insertIndex = i + 1
			}
		}
		if insertIndex != -1 {
			itemToInsert := createItemRef(filledItems[insertIndex-1])
			realItemIndex := filledMap[insertIndex-1]
			filledItems = append(filledItems[:insertIndex], append([]T{itemToInsert}, filledItems[insertIndex:]...)...)
			filledMap = append(filledMap[:insertIndex], append([]int{realItemIndex}, filledMap[insertIndex:]...)...)
		} else {
			break
		}
	}
	return filledItems, filledMap
}

// --- Public Methods ---

// Close gracefully shuts down the buffer's manager goroutine. This method is idempotent
// and blocking, ensuring all resources are released before it returns.
func (b *Buffer[T]) Close() {
	b.closeOnce.Do(func() {
		close(b.quitChan)
	})
	<-b.done
}

// Add sends a request to add a single item to the buffer.
func (b *Buffer[T]) Add(item T) { b.addChan <- item }

// AddAll sends a request to add multiple items to the buffer.
func (b *Buffer[T]) AddAll(items []T) { b.addAllChan <- items }

// GetOldest retrieves the oldest item from the display buffer.
func (b *Buffer[T]) GetOldest() T {
	respChan := make(chan T)
	b.getOldestChan <- respChan
	return <-respChan
}

// TryGetOldest attempts to retrieve the oldest item without blocking.
func (b *Buffer[T]) TryGetOldest() (T, bool) {
	respChan := make(chan tryGetResult[T])
	b.tryGetOldestChan <- respChan
	result := <-respChan
	return result.item, result.ok
}

// GetOldestChan returns a read-only channel for continuous item consumption.
func (b *Buffer[T]) GetOldestChan() <-chan T {
	streamChan := make(chan T, b.size)
	b.registerStreamChan <- streamChan
	return streamChan
}

// GetAll returns a slice of all items currently in the display buffer and clears the buffer.
func (b *Buffer[T]) GetAll() []T {
	respChan := make(chan []T)
	b.getAllChan <- respChan
	return <-respChan
}

// Len returns the current number of items in the display buffer.
func (b *Buffer[T]) Len() int {
	respChan := make(chan int)
	b.lenChan <- respChan
	return <-respChan
}

// Cap returns the configured capacity of the buffer.
func (b *Buffer[T]) Cap() int { return b.size }
