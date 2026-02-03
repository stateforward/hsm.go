# package muid

`import "github.com/stateforward/hsm.go/muid"`

Package muid implements a generator for Monotonically Unique IDs (MUIDs).
MUIDs are 64-bit values inspired by Twitter's Snowflake IDs.
The default layout is:

	[41 bits timestamp (milliseconds since epoch)] [14 bits machine ID] [9 bits counter]

The bit allocation for timestamp, machine ID, and counter, as well as the epoch,
can be customized via the Config struct.
The default epoch starts November 14, 2023 22:13:20 GMT.

- `DefaultConfig`
- `Version` — Version is the current semantic version of the muid package.
- `func MakeString() string` — MakeString generates a new MUID using the default sharded generators and returns it as a base32-encoded string.
- `type Config`
- `type Generator` — Generator is responsible for generating MUIDs.
- `type MUID` — MUID represents a Monotonically Unique ID.

