/*
Package temporalbuffer provides a thread-safe, fixed-size buffer for time-stamped data,
designed to create a perfectly smooth, continuous stream from an irregular or
bursty input.

The package is built with Go Generics, providing compile-time type safety. You create a
buffer for your specific type, and all operations like Add() and GetOldest() work
directly with that type, eliminating the need for type assertions.

Key Features:

  - Type-Safe Generics: Create a buffer for any type that satisfies the DataItem
    interface (i.e., has a CreatedTime() method).

  - Intelligent Smoothing: The default "ResampleTimeline" strategy ensures the buffer's
    contents are always the smoothest possible representation of the real data points.
    Alternative strategies like "FillLargestGap" and "PadWithNewest" are also available.

  - Guaranteed Read Continuity: By default, a read from the buffer is guaranteed
    to return a valid item (either a real one or the last-read item), preventing
    the need for consumer-side timeouts or logic to handle an empty buffer.

  - Highly Configurable: The fill strategy, drop strategy (DropOldest vs.
    DropClosest), and read continuity can all be easily configured via options.

Example: Basic Usage

	// A struct that satisfies the DataItem interface.
	// Note: It also implements ManagedItem for reference counting.
	type Frame struct {
		ID      int
		created time.Time
		// ... other fields and ref-counting logic
	}
	func (f *Frame) CreatedTime() time.Time { return f.created }
	func (f *Frame) Ref() *Frame { return f }
	func (f *Frame) Cleanup()    {}


	// Create a new buffer specifically for *Frame types.
	b := temporalbuffer.New[*Frame](5)
	defer b.Close()

	// Add an item. The buffer will fill to capacity with duplicates.
	b.Add(&Frame{ID: 101, created: time.Now()})

	// In a real application, allow a moment for the buffer to process.
	item := b.GetOldest() // Returns a *Frame directly, no type assertion needed.
	fmt.Printf("Got frame with ID: %d\n", item.ID)

Example: Using a Different Fill Strategy

	// Create a buffer that pads with the newest item instead of resampling.
	b := temporalbuffer.New[*Frame](5,
		temporalbuffer.WithFillStrategy(temporalbuffer.PadWithNewest),
	)
	defer b.Close()

	b.Add(&Frame{ID: 1, created: time.Now()})
	b.Add(&Frame{ID: 2, created: time.Now().Add(1 * time.Second)})

	// The buffer would contain: [A, B, B, B, B]
	// In a real app, allow a moment for processing.
	allItems := b.GetAll()
	fmt.Printf("Buffer length: %d, Second item ID: %d\n", len(allItems), allItems[1].ID)

Example: Simplified Streaming Consumer

	b := temporalbuffer.New[*Frame](5)
	stream := b.GetOldestChan()

	// Consumer
	go func() {
		// The loop variable 'item' is of type *Frame, not DataItem.
		for item := range stream {
			fmt.Printf("Processing item with ID: %d\n", item.ID)
		}
		fmt.Println("Stream closed.")
	}()

	// Producer can add items at any rate.
	b.Add(&Frame{ID: 1, created: time.Now()})

	// When shutting down, close the buffer to also close the stream.
	b.Close()
*/
package temporalbuffer
