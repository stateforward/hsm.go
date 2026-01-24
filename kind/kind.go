package kind

import "sync/atomic"

const (
	length   = 64
	idLength = 8
	depthMax = length / idLength
	idMask   = (1 << idLength) - 1
)

type Kind = uint64

var n uint64

// TypeBases returns the "base" IDs at each level
// (beyond the first) by shifting and masking.
func List(t Kind) [depthMax]Kind {
	var bases [depthMax]Kind
	for i := 1; i < depthMax; i++ {
		bases[i-1] = (t >> (idLength * i)) & idMask
	}
	return bases
}

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

// Is checks if 'kind' matches any or all bases provided.
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
