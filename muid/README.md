# MUID (Micro Universal ID) Package

This package provides a generator for **M**icro **U**niversal **ID**s (MUIDs). MUIDs are 64-bit unique identifiers inspired by Twitter's Snowflake IDs. They are roughly time-sortable and suitable for use as primary keys in distributed systems.

## Features

- **Unique & Sortable:** Generates unique IDs that are approximately ordered by time.
- **High Performance:** Optimized for generating large numbers of IDs quickly, utilizing CPU-based sharding for excellent parallel performance.
- **Concurrency Safe:** Safe for use across multiple goroutines.
- **Configurable:** Allows customization of bit lengths for timestamp, machine ID, and counter, as well as the epoch and machine ID itself via a `Config` struct.

## ID Structure (Default)

Each 64-bit MUID is composed of:

- **Timestamp (41 bits):** Milliseconds since a custom epoch (`1700000000000` - Nov 14, 2023 22:13:20 GMT), allowing for ~69 years of IDs.
- **Machine ID (Variable bits):** Identifier for the machine generating the ID.
- **Shard ID (Variable bits):** Identifier for the specific generator shard within the machine (based on `runtime.NumCPU()`).
- **Counter (9 bits):** Sequence number within the same millisecond on the same machine/shard combination, allowing for 512 unique IDs per millisecond per shard.

**Default Bit Allocation Breakdown:**

The default configuration uses 41 bits for the timestamp and 9 bits for the counter.
The remaining 14 bits (`64 - 41 - 9 = 14`) are dynamically divided between the Machine ID and the Shard ID:

1.  **Shard Bits:** The number of bits needed for the Shard ID is calculated based on the number of CPU cores: `shardBits = ceil(log2(runtime.NumCPU()))`. For example, on an 8-core machine, `shardBits = 3`. On a 1-core machine, `shardBits = 0`.
2.  **Machine Bits:** The remaining bits are used for the Machine ID: `machineBits = 14 - shardBits`. On an 8-core machine, this would be `14 - 3 = 11` bits, allowing for 2048 unique machine IDs per timestamp/counter/shard combination.

This sharding allows the default `Make()` function to achieve high throughput in parallel scenarios by reducing contention.

_Note: The bit allocation for timestamp and counter can still be customized via `Config`, which will affect the total bits available for Machine ID + Shard ID._

## Usage

### Default Generator (Recommended)

The simplest and recommended way to generate an ID is to use the `Make()` function. It utilizes the default configuration and automatically manages a pool of sharded generators based on `runtime.NumCPU()` for optimal performance.

```go
import (
	"fmt"
	"github.com/agentflare-ai/go-muid"
)

func main() {
	// Generate a new MUID using the default sharded generators
	id := muid.Make()

	fmt.Printf("Generated MUID: %s\n", id) // Outputs the base32 representation
	fmt.Printf("Generated MUID (uint64): %d\n", uint64(id))
}
```

### Direct String Generation

For cases where you only need the string representation, use `MakeString()` for convenience:

```go
import (
	"fmt"
	"github.com/agentflare-ai/go-muid"
)

func main() {
	// Generate a new MUID directly as a string
	idStr := muid.MakeString()

	fmt.Printf("Generated MUID: %s\n", idStr) // Direct string output
}
```

`MakeString()` is a convenience function that internally calls `Make().String()`. Use `Make()` when you need both the MUID value and its string representation, or when you want to avoid the string conversion allocation. Use `MakeString()` when you only need the string representation.

### Custom Generator

While `Make()` is generally preferred, you can create individual generator instances if you need fine-grained control or custom configurations. Note that the `NewGenerator` function now requires parameters related to sharding, even if you only intend to create a single instance.

```go
import (
	"fmt"
	"github.com/agentflare-ai/go-muid"
	"time"
)

func main() {
	// Define a custom configuration
	config := muid.Config{
		MachineID:       123,              // Assign a specific machine ID
		TimestampBitLen: 42,              // Use 42 bits for timestamp
		CounterBitLen:   10,              // Use 10 bits for counter
		Epoch:           time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), // Custom epoch
	}

	// For a single custom generator instance, typically use shardIndex 0 and shardBitLen 0.
	// The total bits available for Machine ID are 64 - TimestampBitLen - CounterBitLen - ShardBitLen
	// In this case: 64 - 42 - 10 - 0 = 12 bits.
	shardIndex := uint64(0)
	shardBitLen := 0

	// Create a generator with the custom configuration and shard info.
	// The provided MachineID (123) will be masked to fit the calculated machine bit length (12 bits).
	gen := muid.NewGenerator(config, shardIndex, shardBitLen)

	// Generate an ID using the custom generator
	id := gen.ID()

	fmt.Printf("Generated MUID with custom config: %s\n", id)
}

```

## Performance Comparison

MUID has been benchmarked against other popular unique ID generators. Below is a comprehensive comparison based on performance benchmarks, memory usage, and functional characteristics.

### Benchmark Results (Approximate - Run `go test -bench=. -benchmem` for current results)

| Generator | Generation (ops/sec) | Memory Allocs/op | String Length | Sortable | Collision Safe |
|-----------|---------------------|------------------|---------------|----------|---------------|
| **MUID** | ~45M ops/sec | 0 allocs/op | 13 chars | ✅ Yes | ✅ Yes |
| **MUID String** | ~42M ops/sec | 1 alloc/op | 13 chars | ✅ Yes | ✅ Yes |
| UUID v4 | ~8M ops/sec | 1 alloc/op | 36 chars | ❌ No | ✅ Yes |
| ULID | ~15M ops/sec | 0 allocs/op | 26 chars | ✅ Yes | ✅ Yes |
| NanoID | ~3M ops/sec | 1 alloc/op | 21 chars | ❌ No | ✅ Yes |
| Random uint64 | ~12M ops/sec | 0 allocs/op | N/A | ❌ No | ⚠️ Risk |

*Benchmarks performed on Intel i7-9750H (6 cores) with Go 1.24.5. Results may vary by hardware and Go version.*

### Key Performance Advantages of MUID

1. **Exceptional Speed**: MUID outperforms all other generators in raw generation performance due to its optimized sharding architecture and minimal allocations.

2. **Zero Memory Allocations**: Unlike UUID and NanoID, MUID generates IDs without heap allocations, making it ideal for high-throughput systems.

3. **Compact String Representation**: MUID's base32 encoding produces shorter strings (13 characters) compared to UUID (36 chars) and ULID (26 chars).

4. **Concurrent Performance**: The sharded generator pool provides excellent parallel performance, with minimal contention between goroutines.

5. **Time-Sortable**: MUID maintains approximate temporal ordering, making it suitable for database indexing and time-based queries.

### Detailed Comparison

#### vs UUID v4
- **Performance**: MUID is ~5-6x faster than UUID v4
- **Memory**: MUID uses 0 allocations vs UUID's 1 allocation per ID
- **Size**: MUID strings are 65% shorter (13 vs 36 chars)
- **Sorting**: UUID v4 is not sortable; MUID maintains temporal order
- **Use Case**: Choose MUID for high-performance systems; UUID v4 for cryptographic requirements

#### vs ULID
- **Performance**: MUID is ~3x faster than ULID
- **Size**: MUID strings are 50% shorter (13 vs 26 chars)
- **Sorting**: Both are sortable, but MUID has stricter monotonic guarantees
- **Use Case**: MUID for compact, high-performance IDs; ULID for broader language compatibility

#### vs NanoID
- **Performance**: MUID is ~15x faster than NanoID
- **Memory**: MUID uses 0 allocations vs NanoID's 1+ allocations
- **URL Safety**: Both are URL-safe, but MUID is significantly more efficient
- **Use Case**: MUID for performance-critical applications; NanoID for custom alphabets

#### vs Random uint64
- **Performance**: MUID is ~3-4x faster
- **Functionality**: Random uint64 lacks string representation and sorting capabilities
- **Uniqueness**: MUID provides stronger uniqueness guarantees across distributed systems
- **Use Case**: MUID for distributed systems; random uint64 for simple local use

### Concurrent Performance

MUID's sharded architecture provides excellent concurrent performance:

```
BenchmarkConcurrentMUID-12         10000000    120 ns/op     0 B/op     0 allocs/op
BenchmarkConcurrentUUID-12          500000    2400 ns/op    16 B/op     1 allocs/op
```

The sharding automatically scales with CPU cores, providing optimal performance across different hardware configurations.

## Notes

- The default machine ID calculation (used by `Make()`) uses a hash of the hostname masked to the available _machine ID bits_ (which depend on CPU count as described above).
- If a custom `MachineID` is provided in the `Config` to `NewGenerator`, it will be masked to fit the calculated machine bit length (`64 - TimestampBitLen - CounterBitLen - ShardBitLen`).
- The generator handles counter rollover within the same millisecond by incrementing the timestamp component, ensuring uniqueness even under high burst load per shard.
- The implementation guarantees monotonically increasing timestamps per generator instance. Even if the system clock goes backward, the generator will continue issuing IDs with timestamps based on the last known highest time, ensuring IDs remain sortable by time within that generator's sequence.
- The `Make()` function distributes load across internal generator shards using atomic round-robin selection.
