// Package kind provides a lightweight type identification system using bit-packed uint64 values.
// It enables efficient type checking and inheritance-like relationships through bit manipulation.
// Each Kind can encode up to 8 base types (8 bits each in a 64-bit value), allowing for
// composite types that inherit from multiple bases.
package kind

import "sync/atomic"

const (
	length   = 64         // Total bits in a Kind value
	idLength = 8          // Bits per type ID
	depthMax = length / idLength // Maximum inheritance depth (8 levels)
	idMask   = (1 << idLength) - 1 // Mask for extracting a single ID
)

// Kind is a 64-bit unsigned integer that encodes type identity and inheritance.
// The lowest 8 bits contain the unique type ID, while higher bits contain
// base type IDs for inheritance checking.
type Kind = uint64

// n is the global counter for generating unique type IDs.
var n uint64

// List extracts all base type IDs from a Kind value.
// Returns an array of up to depthMax IDs, where each entry represents
// a base type at that inheritance level. Zero values indicate unused levels.
func List(t Kind) [depthMax]Kind {
	var bases [depthMax]Kind
	for i := 1; i < depthMax; i++ {
		bases[i-1] = (t >> (idLength * i)) & idMask
	}
	return bases
}

// Make creates a new Kind with a unique ID, optionally inheriting from base types.
// The new Kind's ID is stored in the lowest 8 bits, while base type IDs are
// packed into higher bits. Duplicate base IDs are automatically deduplicated.
// This function is thread-safe and uses atomic operations for ID generation.
func Make(bases ...Kind) Kind {
	id := n & idMask
	atomic.AddUint64(&n, 1)
	ids := make(map[Kind]struct{})

	for _, base := range bases {
		for j := 0; j < depthMax; j++ {
			baseId := (base >> (idLength * j)) & idMask
			if baseId == 0 {
				break
			}
			if _, ok := ids[baseId]; !ok {
				ids[baseId] = struct{}{}
				id |= uint64(baseId) << (idLength * len(ids))
			}
		}
	}
	return Kind(id)
}

// Is checks if kind matches any of the provided base types.
// Returns true if kind's ID or any of its inherited base IDs equals
// the ID of any provided base. This enables polymorphic type checking
// similar to instanceof or type assertion in other languages.
//
//go:inline
func Is(kind Kind, bases ...Kind) bool {
	for _, base := range bases {
		baseId := base & idMask
		if kind == baseId {
			return true
		}
		for i := 0; i < depthMax; i++ {
			currentId := (kind >> (idLength * i)) & idMask
			if currentId == baseId {
				return true
			}
		}
	}
	return false
}
