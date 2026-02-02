module github.com/stateforward/hsm.go

go 1.25.3

require (
	github.com/stateforward/hsm.go/kind v1.0.0
	github.com/stateforward/hsm.go/muid v1.0.0
)

replace (
	github.com/stateforward/hsm.go/kind => ./kind
	github.com/stateforward/hsm.go/muid => ./muid
)
