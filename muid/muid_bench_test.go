package muid_test

import (
	"crypto/rand"
	"encoding/binary"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aidarkhanov/nanoid/v2"
	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/stateforward/hsm-go/muid"
)

// BenchmarkMUIDGeneration benchmarks the generation of MUIDs
func BenchmarkMUIDGeneration(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = muid.Make()
	}
}

// BenchmarkMUIDStringGeneration benchmarks the direct generation of MUID strings
func BenchmarkMUIDStringGeneration(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = muid.MakeString()
	}
}

// BenchmarkUUIDv4Generation benchmarks the generation of UUID v4
func BenchmarkUUIDv4Generation(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = uuid.New()
	}
}

// BenchmarkULIDGeneration benchmarks the generation of ULIDs
func BenchmarkULIDGeneration(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ulid.Make()
	}
}

// BenchmarkNanoIDGeneration benchmarks the generation of NanoIDs
func BenchmarkNanoIDGeneration(b *testing.B) {
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = nanoid.GenerateString(alphabet, 21) // 21 chars to match ULID length
	}
}

// BenchmarkRandomUint64Generation benchmarks random uint64 generation
func BenchmarkRandomUint64Generation(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var b [8]byte
		rand.Read(b[:])
		_ = binary.BigEndian.Uint64(b[:])
	}
}

// BenchmarkMUIDString benchmarks string conversion for MUIDs
func BenchmarkMUIDString(b *testing.B) {
	ids := make([]muid.MUID, b.N)
	for i := range ids {
		ids[i] = muid.Make()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ids[i].String()
	}
}

// BenchmarkUUIDString benchmarks string conversion for UUIDs
func BenchmarkUUIDString(b *testing.B) {
	ids := make([]uuid.UUID, b.N)
	for i := range ids {
		ids[i] = uuid.New()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ids[i].String()
	}
}

// BenchmarkULIDString benchmarks string conversion for ULIDs
func BenchmarkULIDString(b *testing.B) {
	ids := make([]ulid.ULID, b.N)
	for i := range ids {
		ids[i] = ulid.Make()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ids[i].String()
	}
}

// BenchmarkNanoIDString benchmarks string conversion for NanoIDs (no-op since it's already a string)
func BenchmarkNanoIDString(b *testing.B) {
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	ids := make([]string, b.N)
	for i := range ids {
		ids[i], _ = nanoid.GenerateString(alphabet, 21)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ids[i] // Already a string
	}
}

// BenchmarkConcurrentMUID benchmarks concurrent MUID generation
func BenchmarkConcurrentMUID(b *testing.B) {
	var wg sync.WaitGroup
	workers := 10
	idsPerWorker := b.N / workers

	b.ResetTimer()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < idsPerWorker; i++ {
				_ = muid.Make()
			}
		}()
	}
	wg.Wait()
}

// BenchmarkConcurrentUUID benchmarks concurrent UUID generation
func BenchmarkConcurrentUUID(b *testing.B) {
	var wg sync.WaitGroup
	workers := 10
	idsPerWorker := b.N / workers

	b.ResetTimer()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < idsPerWorker; i++ {
				_ = uuid.New()
			}
		}()
	}
	wg.Wait()
}

// TestUniqueness tests that all generators produce unique IDs
func TestUniqueness(t *testing.T) {
	const total = 100000

	// Test MUID uniqueness
	t.Run("MUID", func(t *testing.T) {
		muids := make(map[muid.MUID]bool, total)
		for i := 0; i < total; i++ {
			id := muid.Make()
			if muids[id] {
				t.Fatalf("MUID collision detected: %s", id)
			}
			muids[id] = true
		}
	})

	// Test UUID uniqueness
	t.Run("UUID", func(t *testing.T) {
		uuids := make(map[uuid.UUID]bool, total)
		for i := 0; i < total; i++ {
			id := uuid.New()
			if uuids[id] {
				t.Fatalf("UUID collision detected: %s", id)
			}
			uuids[id] = true
		}
	})

	// Test ULID uniqueness
	t.Run("ULID", func(t *testing.T) {
		ulids := make(map[ulid.ULID]bool, total)
		for i := 0; i < total; i++ {
			id := ulid.Make()
			if ulids[id] {
				t.Fatalf("ULID collision detected: %s", id)
			}
			ulids[id] = true
		}
	})

	// Test NanoID uniqueness
	t.Run("NanoID", func(t *testing.T) {
		alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
		nanoids := make(map[string]bool, total)
		for i := 0; i < total; i++ {
			id, _ := nanoid.GenerateString(alphabet, 21)
			if nanoids[id] {
				t.Fatalf("NanoID collision detected: %s", id)
			}
			nanoids[id] = true
		}
	})
}

// TestSortability tests that IDs maintain temporal ordering
func TestSortability(t *testing.T) {
	const total = 10000

	// Test MUID sortability
	t.Run("MUID", func(t *testing.T) {
		var ids []muid.MUID
		for i := 0; i < total; i++ {
			ids = append(ids, muid.Make())
			time.Sleep(time.Microsecond) // Ensure different timestamps
		}

		if !sort.SliceIsSorted(ids, func(i, j int) bool {
			return uint64(ids[i]) < uint64(ids[j])
		}) {
			t.Fatal("MUIDs are not sortable")
		}
	})

	// Test ULID sortability
	t.Run("ULID", func(t *testing.T) {
		var ids []ulid.ULID
		for i := 0; i < total; i++ {
			ids = append(ids, ulid.Make())
			time.Sleep(time.Microsecond) // Ensure different timestamps
		}

		if !sort.SliceIsSorted(ids, func(i, j int) bool {
			return ids[i].Compare(ids[j]) < 0
		}) {
			t.Fatal("ULIDs are not sortable")
		}
	})
}

// BenchmarkMemoryMUID benchmarks memory allocations for MUID generation
func BenchmarkMemoryMUID(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = muid.Make()
	}
}

// BenchmarkMemoryUUID benchmarks memory allocations for UUID generation
func BenchmarkMemoryUUID(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = uuid.New()
	}
}

// BenchmarkMemoryULID benchmarks memory allocations for ULID generation
func BenchmarkMemoryULID(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ulid.Make()
	}
}

// BenchmarkMemoryNanoID benchmarks memory allocations for NanoID generation
func BenchmarkMemoryNanoID(b *testing.B) {
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = nanoid.GenerateString(alphabet, 21)
	}
}
