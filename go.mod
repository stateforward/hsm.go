module github.com/stateforward/hsm.go

go 1.25.3

require (
	github.com/stateforward/hsm.go/kind v0.0.0-20260124060507-9cca10687b7c
	github.com/stateforward/hsm.go/muid v0.0.0-20260124060507-9cca10687b7c
)

replace (
	github.com/stateforward/hsm.go/kind => ./kind
	github.com/stateforward/hsm.go/muid => ./muid
)
