# kind

A high-performance hierarchical type system for Go using bit manipulation and uint64 values. This package provides an efficient way to create, manage, and check hierarchical relationships between types/kinds.

## Features

- **Hierarchical Types**: Create complex type hierarchies with multiple inheritance
- **Bit Manipulation**: Efficient storage using uint64 with 8-bit chunks (up to 8 levels deep)
- **Type Checking**: Fast membership testing for hierarchical relationships
- **Memory Efficient**: Single uint64 per kind with no additional allocations
- **Thread-Safe**: Global counter for unique ID generation
- **Zero Dependencies**: Pure Go standard library implementation

## Installation

```bash
go get github.com/agentflare-ai/go-kind@main
```

## Quick Start

```go
package main

import (
    "fmt"
    "github.com/agentflare-ai/go-kind"
)

func main() {
    // Create base types
    animal := kind.Make()     // Creates a new unique kind
    mammal := kind.Make(animal) // Inherits from animal
    dog := kind.Make(mammal)  // Inherits from mammal (and animal)
    cat := kind.Make(mammal)  // Inherits from mammal (and animal)

    // Check relationships
    fmt.Println(kind.Is(dog, animal)) // true - dog is an animal
    fmt.Println(kind.Is(dog, mammal)) // true - dog is a mammal
    fmt.Println(kind.Is(dog, cat))    // false - dog is not a cat
    fmt.Println(kind.Is(cat, mammal)) // true - cat is a mammal

    // List inheritance hierarchy
    bases := kind.List(dog)
    fmt.Printf("Dog inherits from: %v\n", bases)
}
```

## Core Concepts

### Kind Type
- `Kind` is an alias for `uint64`
- Each kind represents a unique type identifier
- Hierarchical relationships are encoded in the bit structure

### Bit Structure
The 64-bit kind value is divided into 8-byte chunks:
```
Bits:  [63-56] [55-48] [47-40] [39-32] [31-24] [23-16] [15-8] [7-0]
Level:    7       6       5       4       3       2      1     0
```
- Level 0: The kind's own unique ID
- Levels 1-7: Inherited base kind IDs

## API Reference

### Functions

#### `Make(bases ...Kind) Kind`
Creates a new unique kind that inherits from the provided base kinds.

```go
// Create base types
vehicle := kind.Make()
car := kind.Make(vehicle)
truck := kind.Make(vehicle)

// Create derived types
sedan := kind.Make(car)
pickup := kind.Make(truck)
```

#### `Is(kind Kind, bases ...Kind) bool`
Checks if a kind matches any of the provided base kinds, including hierarchical relationships.

```go
// Direct match
kind.Is(sedan, car) // true

// Hierarchical match
kind.Is(sedan, vehicle) // true

// Multiple bases (OR logic)
kind.Is(sedan, car, truck) // true
```

#### `List(kind Kind) [depthMax]Kind`
Returns an array of all base kinds that this kind inherits from.

```go
bases := kind.List(sedan)
// Returns: [car, vehicle, 0, 0, 0, 0, 0, 0]
```

### Constants

- `depthMax = 8`: Maximum inheritance depth
- `idLength = 8`: Bits per ID chunk
- `idMask = 255`: Mask for extracting ID chunks

## Advanced Usage

### Complex Hierarchies

```go
// Create a complex type hierarchy
entity := kind.Make()
living := kind.Make(entity)
animal := kind.Make(living)
mammal := kind.Make(animal)
primate := kind.Make(mammal)
human := kind.Make(primate)

// Check deep inheritance
kind.Is(human, entity)  // true
kind.Is(human, living)  // true
kind.Is(human, animal)  // true
kind.Is(human, mammal)  // true
kind.Is(human, primate) // true
```

### Multiple Inheritance

```go
// Types can inherit from multiple bases
flying := kind.Make(entity)
bird := kind.Make(animal, flying) // Bird is both animal and flying

kind.Is(bird, animal) // true
kind.Is(bird, flying) // true
kind.Is(bird, living) // true (through animal)
```

### Type Registry Pattern

```go
// Common pattern: Define types as constants
const (
    Entity Kind = iota + 1
    Living
    Animal
    Plant
    Mammal
    Bird
    Fish
)

// Initialize with relationships
func init() {
    // This would require extending the API to support predefined IDs
    // For now, use the Make function approach
}
```

## Performance Characteristics

- **Space**: 8 bytes per kind (uint64)
- **Time**: O(1) for `Is()` checks, O(depth) for `Make()` and `List()`
- **Memory**: Zero allocations for core operations
- **Thread Safety**: Global counter is not thread-safe (by design for performance)

## Limitations

- Maximum 8 levels of inheritance depth
- Maximum 255 unique IDs per level (0 is reserved)
- Global counter is not thread-safe
- No runtime type reflection integration

## Testing

Run the test suite:

```bash
go test ./...
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Add comprehensive tests
4. Ensure all tests pass
5. Submit a pull request

## Use Cases

- **Entity Component Systems**: Type hierarchies for game entities
- **Plugin Systems**: Categorizing plugins with inheritance
- **Authorization**: Hierarchical permission systems
- **Content Management**: Content type hierarchies
- **API Design**: Resource type relationships

## License

Private - Agentflare AI Internal Use Only

## Dependencies

None - pure Go standard library implementation
