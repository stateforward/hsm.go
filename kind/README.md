# package kind

`import "github.com/stateforward/hsm.go/kind"`

Package kind provides a lightweight type identification system using bit-packed uint64 values.
It enables efficient type checking and inheritance-like relationships through bit manipulation.
Each Kind can encode up to 8 base types (8 bits each in a 64-bit value), allowing for
composite types that inherit from multiple bases.

- `Version` — Version is the current semantic version of the kind package.
- `func Is(kind Kind, bases ...Kind) bool` — Is checks if kind matches any of the provided base types.
- `type Kind` — Kind is a 64-bit unsigned integer that encodes type identity and inheritance.

