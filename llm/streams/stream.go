package streams

// Stream represents a generic stream interface
// The caller should check the Err() method to ensure there's no error.
type Stream[T any] interface {
	// Next indicate if there's a next item.
	Next() bool
	// Current returns the current event
	Current() T
	// Err returns any error that occurred
	Err() error
	// Close closes the stream
	Close() error
}

// Interruptible is implemented by the lowest stream layer that can safely
// interrupt a blocking Next from another goroutine. Outer transform and
// persistence wrappers must finish Next/Current before their Close runs.
type Interruptible interface {
	Interrupt() error
}
