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

// DataItem is the required interface for any item stored in the buffer.
type DataItem interface {
	CreatedTime() time.Time
}

// Cleanable is an optional interface for items that require cleanup logic.
type Cleanable interface {
	Cleanup()
}

// Referenceable is an optional interface for items that define how they should be duplicated.
type Referenceable interface {
	Ref() DataItem
}

// --- Internal Types ---

type tryGetResult struct {
	item DataItem
	ok   bool
}

// bufferState holds the internal, mutable state of the buffer, managed exclusively by the run goroutine.
type bufferState struct {
	realItems        []DataItem
	displayItems     []DataItem
	lastReadItem     DataItem
	streamOutputChan chan<- DataItem
	opts             options
	size             int
}

// --- Buffer Implementation ---

// Buffer is a thread-safe, fixed-size buffer for time-stamped data. It serializes
// all access through a central manager goroutine.
type Buffer struct {
	size int
	opts options

	addChan            chan DataItem
	addAllChan         chan []DataItem
	getOldestChan      chan chan DataItem
	tryGetOldestChan   chan chan tryGetResult
	getAllChan         chan chan []DataItem
	registerStreamChan chan chan DataItem
	lenChan            chan chan int
	quitChan           chan struct{}
	closeOnce          sync.Once
}

// New creates a new Buffer with the given size and optional configurations.
// It spawns a manager goroutine that is cleaned up when Close() is called.
func New(size int, opts ...Option) *Buffer {
	cfg := options{
		fillStrategy:   ResampleTimeline,
		readContinuity: true,
		dropStrategy:   DropClosest,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	b := &Buffer{
		size:               size,
		opts:               cfg,
		addChan:            make(chan DataItem),
		addAllChan:         make(chan []DataItem),
		getOldestChan:      make(chan chan DataItem),
		tryGetOldestChan:   make(chan chan tryGetResult),
		getAllChan:         make(chan chan []DataItem),
		registerStreamChan: make(chan chan DataItem),
		lenChan:            make(chan chan int),
		quitChan:           make(chan struct{}),
	}
	go b.run()
	return b
}

// run is the manager goroutine that owns and manages the buffer state. It is the
// only goroutine that ever accesses the internal slices, which guarantees thread-safety.
func (b *Buffer) run() {
	state := &bufferState{
		realItems:    make([]DataItem, 0, b.size),
		displayItems: make([]DataItem, 0, b.size),
		opts:         b.opts,
		size:         b.size,
	}

	for {
		// To reduce complexity, we use a single select block. Channels that should not be
		// active are set to nil, which effectively disables their case statements.
		var oldestChan chan chan DataItem
		var streamChan chan<- DataItem
		var streamValue DataItem

		canServe := len(state.displayItems) > 0 || (state.opts.readContinuity && state.lastReadItem != nil)
		if canServe {
			oldestChan = b.getOldestChan
			streamChan = state.streamOutputChan
			if len(state.displayItems) > 0 {
				streamValue = state.displayItems[0]
			} else {
				streamValue = state.createRef(state.lastReadItem)
			}
		}

		select {
		case item := <-b.addChan:
			state.handleAdd(item)
		case newItems := <-b.addAllChan:
			state.handleAddAll(newItems)
		case respChan := <-oldestChan: // Will be nil if buffer can't serve
			state.handleGetOldest(respChan)
		case respChan := <-b.tryGetOldestChan:
			state.handleTryGetOldest(respChan)
		case respChan := <-b.getAllChan:
			state.handleGetAll(respChan)
		case reqChan := <-b.registerStreamChan:
			state.streamOutputChan = reqChan
		case streamChan <- streamValue: // Will be nil if buffer can't serve
			state.handleStreamSend()
		case respChan := <-b.lenChan:
			respChan <- len(state.displayItems)
		case <-b.quitChan:
			cleanup(state.realItems, state.streamOutputChan)
			return
		}
	}
}

// --- State Handlers ---

// rebuildDisplayBuffer recalculates the user-facing slice based on the real items
// and the configured drop/fill strategies.
func (s *bufferState) rebuildDisplayBuffer() {
	s.realItems = applyDropStrategy(s.realItems, s.size, s.opts.dropStrategy)
	s.displayItems = applyFillStrategy(s.realItems, s.size, s.opts.fillStrategy)
}

// createRef is a helper to safely call the Ref() method if the item implements it.
func (s *bufferState) createRef(item DataItem) DataItem {
	if item == nil {
		return nil
	}
	if ref, ok := item.(Referenceable); ok {
		return ref.Ref()
	}
	return item
}

func (s *bufferState) handleAdd(item DataItem) {
	s.realItems = append(s.realItems, item)
	s.rebuildDisplayBuffer()
}

func (s *bufferState) handleAddAll(items []DataItem) {
	s.realItems = append(s.realItems, items...)
	s.rebuildDisplayBuffer()
}

func (s *bufferState) handleGetOldest(respChan chan DataItem) {
	if len(s.displayItems) > 0 {
		itemToReturn := s.displayItems[0]
		s.lastReadItem = itemToReturn
		respChan <- itemToReturn
		if len(s.realItems) > 0 {
			s.realItems = s.realItems[1:]
		}
		s.rebuildDisplayBuffer()
	} else {
		// Buffer is empty, but we can provide a continuity item.
		respChan <- s.createRef(s.lastReadItem)
	}
}

func (s *bufferState) handleTryGetOldest(respChan chan tryGetResult) {
	if len(s.displayItems) > 0 {
		itemToReturn := s.displayItems[0]
		s.lastReadItem = itemToReturn
		respChan <- tryGetResult{item: itemToReturn, ok: true}
		if len(s.realItems) > 0 {
			s.realItems = s.realItems[1:]
		}
		s.rebuildDisplayBuffer()
	} else if s.opts.readContinuity && s.lastReadItem != nil {
		// Buffer is empty, provide continuity item.
		respChan <- tryGetResult{item: s.createRef(s.lastReadItem), ok: true}
	} else {
		// No items and no continuity possible, so fail the request.
		respChan <- tryGetResult{item: nil, ok: false}
	}
}

func (s *bufferState) handleGetAll(respChan chan []DataItem) {
	if len(s.displayItems) == 0 {
		respChan <- nil
	} else {
		respChan <- s.displayItems
	}
	// GetAll is a destructive read.
	s.realItems = make([]DataItem, 0, s.size)
	s.displayItems = make([]DataItem, 0, s.size)
}

func (s *bufferState) handleStreamSend() {
	if len(s.displayItems) > 0 {
		s.lastReadItem = s.displayItems[0]
		// If we sent a real item, remove it from the source list.
		if len(s.realItems) > 0 {
			s.realItems = s.realItems[1:]
		}
	}
	s.rebuildDisplayBuffer()
}

// --- Pure Functions ---

// cleanup handles resource cleanup on shutdown.
func cleanup(items []DataItem, streamChan chan<- DataItem) {
	for _, item := range items {
		if cleanable, ok := item.(Cleanable); ok {
			cleanable.Cleanup()
		}
	}
	// Closing the stream channel signals to consumers that no more items will be sent.
	if streamChan != nil {
		close(streamChan)
	}
}

// applyDropStrategy is a pure function that sorts and drops items if over capacity.
func applyDropStrategy(items []DataItem, size int, strategy DropStrategy) []DataItem {
	if len(items) <= size {
		return items
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedTime().Before(items[j].CreatedTime())
	})

	switch strategy {
	case DropOldest:
		droppedCount := len(items) - size
		dropped := items[:droppedCount]
		items = items[droppedCount:]
		for _, item := range dropped {
			if cleanable, ok := item.(Cleanable); ok {
				cleanable.Cleanup()
			}
		}
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
				if cleanable, ok := itemToDrop.(Cleanable); ok {
					cleanable.Cleanup()
				}
			} else {
				// Failsafe to prevent infinite loop.
				break
			}
		}
	}
	return items
}

// applyFillStrategy has been refactored to dispatch to smaller helper functions.
func applyFillStrategy(items []DataItem, size int, strategy FillStrategy) []DataItem {
	if len(items) == 0 || len(items) >= size {
		return items
	}
	switch strategy {
	case ResampleTimeline:
		return fillResampleTimeline(items, size)
	case PadWithNewest:
		return fillPadWithNewest(items, size)
	case FillLargestGap:
		return fillLargestGap(items, size)
	case NoFill:
		// Do nothing
	}
	return items
}

func fillResampleTimeline(items []DataItem, size int) []DataItem {
	resampled := make([]DataItem, 0, size)
	numItems := len(items)
	baseCount := size / numItems
	extraCount := size % numItems
	createRef := func(item DataItem) DataItem {
		if ref, ok := item.(Referenceable); ok {
			return ref.Ref()
		}
		return item
	}
	for i, item := range items {
		count := baseCount
		if i < extraCount {
			count++
		}
		for j := 0; j < count; j++ {
			resampled = append(resampled, createRef(item))
		}
	}
	return resampled
}

func fillPadWithNewest(items []DataItem, size int) []DataItem {
	filledItems := make([]DataItem, len(items), size)
	copy(filledItems, items)
	newestItem := items[len(items)-1]
	createRef := func(item DataItem) DataItem {
		if ref, ok := item.(Referenceable); ok {
			return ref.Ref()
		}
		return item
	}
	for i := len(items); i < size; i++ {
		filledItems = append(filledItems, createRef(newestItem))
	}
	return filledItems
}

func fillLargestGap(items []DataItem, size int) []DataItem {
	filledItems := make([]DataItem, len(items))
	copy(filledItems, items)
	createRef := func(item DataItem) DataItem {
		if ref, ok := item.(Referenceable); ok {
			return ref.Ref()
		}
		return item
	}
	for len(filledItems) < size {
		if len(filledItems) < 2 {
			if len(filledItems) == 1 {
				filledItems = append(filledItems, createRef(filledItems[0]))
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
			itemToInsert := createRef(filledItems[insertIndex-1])
			filledItems = append(filledItems[:insertIndex], append([]DataItem{itemToInsert}, filledItems[insertIndex:]...)...)
		} else {
			break
		}
	}
	return filledItems
}

// --- Public Methods ---

// Close gracefully shuts down the buffer's manager goroutine. This method is idempotent
// and is safe to call multiple times.
func (b *Buffer) Close() {
	b.closeOnce.Do(func() {
		close(b.quitChan)
	})
}

// Add sends a request to add a single item to the buffer.
func (b *Buffer) Add(item DataItem) {
	b.addChan <- item
}

// AddAll sends a request to add multiple items to the buffer.
func (b *Buffer) AddAll(items []DataItem) {
	b.addAllChan <- items
}

// GetOldest retrieves the oldest item from the display buffer.
func (b *Buffer) GetOldest() DataItem {
	respChan := make(chan DataItem)
	b.getOldestChan <- respChan
	return <-respChan
}

// TryGetOldest attempts to retrieve the oldest item without blocking.
func (b *Buffer) TryGetOldest() (DataItem, bool) {
	respChan := make(chan tryGetResult)
	b.tryGetOldestChan <- respChan
	result := <-respChan
	return result.item, result.ok
}

// GetOldestChan returns a read-only channel for continuous item consumption.
func (b *Buffer) GetOldestChan() <-chan DataItem {
	streamChan := make(chan DataItem, b.size)
	b.registerStreamChan <- streamChan
	return streamChan
}

// GetAll returns a slice of all items currently in the display buffer and clears the real items.
func (b *Buffer) GetAll() []DataItem {
	respChan := make(chan []DataItem)
	b.getAllChan <- respChan
	return <-respChan
}

// Len returns the current number of items in the display buffer.
func (b *Buffer) Len() int {
	respChan := make(chan int)
	b.lenChan <- respChan
	return <-respChan
}

// Cap returns the configured capacity of the buffer.
func (b *Buffer) Cap() int {
	return b.size
}
