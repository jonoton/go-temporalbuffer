/*
Package temporalbuffer provides a thread-safe, fixed-size buffer for time-stamped data,
designed to create a perfectly smooth, continuous stream from an irregular or
bursty input.

The buffer's core feature is "Timeline Resampling." By default, it intelligently
fills empty space by proportionally representing the real items it contains. For example,
if you have 2 items in a buffer of 10, the buffer will fill itself with 5 copies of
the first item followed by 5 of the second, creating an evenly distributed timeline.
This makes it ideal for applications like video frame buffering or smoothing out
real-time sensor data for display.

Key Features:

- Intelligent Smoothing: The default "ResampleTimeline" strategy ensures the buffer's
contents are always the smoothest possible representation of the real data points.
Alternative strategies like "FillLargestGap" and "PadWithNewest" are also available.

- Guaranteed Read Continuity: By default, a read from the buffer is guaranteed
to return a valid item (either a real one or the last-read item), preventing
the need for consumer-side timeouts or logic to handle an empty buffer.

- Highly Configurable: The fill strategy, drop strategy (DropOldest vs.
DropClosest), and read continuity can all be easily configured via options.

- Thread-Safe and Streaming-Friendly: Built for concurrent use with a simple,
powerful channel-based API for building robust consumer pipelines.

Example: Default Resampling Behavior

	// A struct that implements the DataItem interface.
	type Event struct {
		Message string
		Time    time.Time
	}
	func (e Event) CreatedTime() time.Time { return e.Time }

	// Create a buffer of size 5 with default settings (ResampleTimeline).
	b := temporalbuffer.New(5)
	defer b.Close()

	// Add two items with different timestamps.
	b.Add(Event{Message: "A", Time: time.Now()})
	b.Add(Event{Message: "B", Time: time.Now().Add(10 * time.Second)})

	// The buffer will resample its contents to be as evenly-spaced as possible.
	// In a real app, allow a moment for the buffer to process.
	allItems := b.GetAll()
	// allItems will contain a mix of A's and B's, proportionally
	// distributed to fill the 5 slots (e.g., [A, A, A, B, B]).
	fmt.Printf("Buffer length: %d\n", len(allItems)) // Prints: 5

Example: Using FillLargestGap Strategy

	// Create a buffer that fills the largest gaps first.
	b := temporalbuffer.New(5,
		temporalbuffer.WithFillStrategy(temporalbuffer.FillLargestGap),
	)
	defer b.Close()
	b.Add(Event{Message: "A", Time: time.Now()})
	b.Add(Event{Message: "B", Time: time.Now().Add(10 * time.Second)})

	// The buffer would contain: [A, A, A, A, B]
	// as it fills the single large gap with duplicates of "A".

Example: Simplified Streaming Consumer

	b := temporalbuffer.New(5) // Defaults are perfect for this
	stream := b.GetOldestChan()

	// Consumer
	go func() {
		// This loop is robust. It will receive a continuous stream of items,
		// either real or resampled, and will never get an empty read
		// thanks to the default ReadContinuity feature.
		for item := range stream {
			fmt.Printf("Processing item: %s\n", item.(Event).Message)
		}
		fmt.Println("Stream closed.")
	}()

	// Producer can add items at any rate.
	b.Add(Event{Message: "A", Time: time.Now()})

	// When shutting down, close the buffer to also close the stream.
	b.Close()
*/
package temporalbuffer
