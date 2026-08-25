// Package buffer provides a concurrency-safe, byte-bounded circular buffer.
package buffer

import "sync"

// Buffer retains only the newest bytes written to it. It implements
// io.Writer and is safe for concurrent readers and writers.
type Buffer struct {
	mu     sync.RWMutex
	data   []byte
	start  int
	length int
}

// New creates a Buffer with the requested byte capacity. Non-positive
// capacities are clamped to one byte.
func New(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	return &Buffer{data: make([]byte, capacity)}
}

// Capacity returns the maximum number of bytes retained by the buffer.
func (b *Buffer) Capacity() int {
	return len(b.data)
}

// Len returns the number of bytes currently retained by the buffer.
func (b *Buffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.length
}

// Write implements io.Writer. It reports the full input length even when old
// or leading bytes are discarded to respect the fixed capacity.
func (b *Buffer) Write(p []byte) (int, error) {
	written := len(p)
	if written == 0 {
		return 0, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	capacity := len(b.data)
	if len(p) >= capacity {
		copy(b.data, p[len(p)-capacity:])
		b.start = 0
		b.length = capacity
		return written, nil
	}

	end := (b.start + b.length) % capacity
	first := min(len(p), capacity-end)
	copy(b.data[end:], p[:first])
	copy(b.data, p[first:])

	if b.length+len(p) <= capacity {
		b.length += len(p)
	} else {
		overflow := b.length + len(p) - capacity
		b.start = (b.start + overflow) % capacity
		b.length = capacity
	}

	return written, nil
}

// Bytes returns the retained bytes in write order as an independent copy.
func (b *Buffer) Bytes() []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]byte, b.length)
	if b.length == 0 {
		return result
	}

	first := min(b.length, len(b.data)-b.start)
	copy(result, b.data[b.start:b.start+first])
	copy(result[first:], b.data[:b.length-first])
	return result
}

// String returns the retained bytes in write order as a string.
func (b *Buffer) String() string {
	return string(b.Bytes())
}
