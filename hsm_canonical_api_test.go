package hsm_test

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stateforward/hsm.go"
)

func TestCanonicalKindAPIs(t *testing.T) {
	base := hsm.MakeKind()
	derived := hsm.MakeKind(base)

	if !hsm.IsKind(derived, base) {
		t.Fatal("IsKind() should report derived kind as base kind")
	}
	if !hsm.IsKind(derived, derived) {
		t.Fatal("IsKind() should report kind as itself")
	}
	if hsm.IsKind(base, derived) {
		t.Fatal("IsKind() should not report base kind as derived kind")
	}
}

func TestRuntimeIndexesBelongToFinalizedModel(t *testing.T) {
	modelType := reflect.TypeOf(hsm.Model{})
	for _, fieldName := range []string{"TransitionMap", "DeferredMap", "TransitionPaths", "HistoryPaths", "HistoryTargets", "transitionMap", "deferredMap", "transitionPaths", "historyPaths", "historyTargets"} {
		if _, ok := modelType.FieldByName(fieldName); ok {
			t.Fatalf("Model exposes runtime index field %q", fieldName)
		}
	}

	finalizedType := reflect.TypeOf(hsm.FinalizedModel{})
	for _, fieldName := range []string{"transitionMap", "deferredMap", "transitionPaths", "historyPaths", "historyTargets"} {
		field, ok := finalizedType.FieldByName(fieldName)
		if !ok {
			t.Fatalf("FinalizedModel missing runtime index field %q", fieldName)
		}
		if field.IsExported() {
			t.Fatalf("FinalizedModel runtime index field %q should be private", fieldName)
		}
	}
}

func TestNewRequiresFinalizedModelFromBuilder(t *testing.T) {
	assertPanicContains(t, "zero finalized model", "finalized model is required", func() {
		var model hsm.FinalizedModel
		hsm.New(&THSM{}, &model)
	})
	assertPanicContains(t, "nil finalized model", "finalized model is required", func() {
		hsm.New(&THSM{}, (*hsm.FinalizedModel)(nil))
	})
}

func TestOnAcceptsStringEventNames(t *testing.T) {
	type eventName string

	model := hsm.Define(
		"CanonicalStringOnHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State(
			"idle",
			hsm.Transition(hsm.On("go"), hsm.Target("../done")),
		),
		hsm.State("done"),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)

	<-hsm.Dispatch(context.Background(), sm, hsm.Event{Name: "go"})
	if got := sm.State(); got != "/CanonicalStringOnHSM/done" {
		t.Fatalf("string On transition state = %q, want done", got)
	}

	aliasModel := hsm.Define(
		"CanonicalStringAliasOnHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State(
			"idle",
			hsm.Transition(hsm.On(eventName("advance")), hsm.Target("../done")),
		),
		hsm.State("done"),
	)
	aliasSM := hsm.Started(context.Background(), &THSM{}, &aliasModel)

	<-hsm.Dispatch(context.Background(), aliasSM, hsm.Event{Name: "advance"})
	if got := aliasSM.State(); got != "/CanonicalStringAliasOnHSM/done" {
		t.Fatalf("string alias On transition state = %q, want done", got)
	}
}

func TestMakeGroupAliasesNewGroupAndSupportsLeadingID(t *testing.T) {
	model := hsm.Define(
		"CanonicalGroupHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle"),
	)
	first := hsm.New(&THSM{}, &model)
	second := hsm.New(&THSM{}, &model)
	nested := hsm.NewGroup(second)

	group := hsm.MakeGroup("workers", first, nil, nested)

	if got := hsm.ID(group); got != "workers" {
		t.Fatalf("ID(MakeGroup(...)) = %q, want workers", got)
	}
	if got := len(group.Instances()); got != 2 {
		t.Fatalf("MakeGroup instance count = %d, want 2", got)
	}
}

func TestGroupStatesAndSnapshotsPreserveOrderThroughLifecycle(t *testing.T) {
	advance := hsm.Event{Name: "advance"}
	model := hsm.Define(
		"CanonicalGroupSnapshotHSM",
		hsm.Attribute("flag", false),
		hsm.Initial(hsm.Target("idle")),
		hsm.State(
			"idle",
			hsm.Transition(hsm.On(advance), hsm.Target("../done")),
		),
		hsm.State("done"),
	)
	first := hsm.New(&THSM{}, &model, hsm.Config{ID: "first"})
	second := hsm.New(&THSM{}, &model, hsm.Config{ID: "second"})
	group := hsm.Start(context.Background(), hsm.MakeGroup("workers", first, second))

	assertStates := func(label string, states []string, want ...string) {
		t.Helper()
		if len(states) != len(want) {
			t.Fatalf("%s states length = %d, want %d: %v", label, len(states), len(want), states)
		}
		for index, expected := range want {
			if states[index] != expected {
				t.Fatalf("%s state[%d] = %q, want %q; all states=%v", label, index, states[index], expected, states)
			}
		}
	}
	assertSnapshotOrder := func(label string, snapshots []hsm.Snapshot, wantIDs []string, wantStates []string) {
		t.Helper()
		if len(snapshots) != len(wantIDs) {
			t.Fatalf("%s snapshot length = %d, want %d: %+v", label, len(snapshots), len(wantIDs), snapshots)
		}
		for index, snapshot := range snapshots {
			if snapshot.ID != wantIDs[index] {
				t.Fatalf("%s snapshot[%d].ID = %q, want %q", label, index, snapshot.ID, wantIDs[index])
			}
			if snapshot.State != wantStates[index] {
				t.Fatalf("%s snapshot[%d].State = %q, want %q", label, index, snapshot.State, wantStates[index])
			}
		}
	}

	idle := "/CanonicalGroupSnapshotHSM/idle"
	done := "/CanonicalGroupSnapshotHSM/done"
	root := "/CanonicalGroupSnapshotHSM"
	assertStates("after start", group.States(), idle, idle)
	assertSnapshotOrder("after start", hsm.TakeSnapshot(context.Background(), group), []string{"first", "second"}, []string{idle, idle})

	awaitWaiter(t, "group dispatch", hsm.Dispatch(group.Context(), group, advance))
	assertStates("after dispatch", group.States(), done, done)
	assertSnapshotOrder("after dispatch", hsm.TakeSnapshot(context.Background(), group), []string{"first", "second"}, []string{done, done})

	awaitWaiter(t, "group restart", hsm.Restart(group.Context(), group))
	assertStates("after restart", group.States(), idle, idle)
	assertSnapshotOrder("after restart", group.Snapshots(), []string{"first", "second"}, []string{idle, idle})
	restartedInstances, ok := hsm.InstancesFromContext(group.Context())
	if !ok || len(restartedInstances) != 2 {
		t.Fatalf("InstancesFromContext(group.Context()) after restart = (%v, %v), want two members", restartedInstances, ok)
	}
	restartedIDs := map[string]bool{}
	for _, instance := range restartedInstances {
		restartedIDs[hsm.ID(instance)] = true
	}
	if !restartedIDs["first"] || !restartedIDs["second"] {
		t.Fatalf("group context registry after restart = %v, want first and second", restartedIDs)
	}
	awaitWaiter(t, "dispatch all after group restart", hsm.DispatchAll(group.Context(), advance))
	assertStates("after restart dispatch all", group.States(), done, done)

	awaitWaiter(t, "group stop", hsm.Stop(context.Background(), group))
	assertStates("after stop", group.States(), "", "")
	assertSnapshotOrder("after stop", hsm.TakeSnapshot(context.Background(), group), []string{"first", "second"}, []string{root, root})
}

func TestGroupStartupDataIsClonedPerMember(t *testing.T) {
	type startupCloneHSM struct {
		hsm.HSM
	}
	type startupSeen struct {
		id      string
		members []string
	}
	seen := make(chan startupSeen, 4)
	model := hsm.Define(
		"GroupStartupCloneHSM",
		hsm.Initial(
			hsm.Target("idle"),
			hsm.Effect(func(ctx context.Context, sm *startupCloneHSM, event hsm.Event) {
				data := event.Data.(map[string][]string)
				data["members"] = append(data["members"], hsm.ID(sm))
				seen <- startupSeen{id: hsm.ID(sm), members: append([]string(nil), data["members"]...)}
			}),
		),
		hsm.State("idle"),
	)
	assertRecords := func(label string) {
		t.Helper()
		got := map[string][]string{}
		for range 2 {
			select {
			case record := <-seen:
				got[record.id] = record.members
			case <-time.After(2 * time.Second):
				t.Fatalf("%s timed out waiting for startup records; got %#v", label, got)
			}
		}
		for _, id := range []string{"first", "second"} {
			members, ok := got[id]
			if !ok {
				t.Fatalf("%s missing startup record for %s: %#v", label, id, got)
			}
			if len(members) != 1 || members[0] != id {
				t.Fatalf("%s startup record for %s saw shared data %#v", label, id, members)
			}
		}
	}

	first := hsm.New(&startupCloneHSM{}, &model, hsm.Config{ID: "first"})
	second := hsm.New(&startupCloneHSM{}, &model, hsm.Config{ID: "second"})
	startData := map[string][]string{"members": []string{}}
	group := hsm.Start(context.Background(), hsm.MakeGroup("workers", first, second), startData)
	assertRecords("start")
	if len(startData["members"]) != 0 {
		t.Fatalf("start data mutated by group startup: %#v", startData)
	}

	restartData := map[string][]string{"members": []string{}}
	awaitWaiter(t, "group startup clone restart", hsm.Restart(group.Context(), group, restartData))
	assertRecords("restart")
	if len(restartData["members"]) != 0 {
		t.Fatalf("restart data mutated by group restart: %#v", restartData)
	}
}

type groupBehaviorWorker struct {
	hsm.HSM
	seen atomic.Int64
}

func TestGroupCanBeUsedAsBehaviorValue(t *testing.T) {
	event := hsm.Event{Name: "Go"}
	workerModel := hsm.Define(
		"GroupBehaviorWorkerHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(event),
				hsm.Effect(func(_ context.Context, sm *groupBehaviorWorker, _ hsm.Event) {
					sm.seen.Add(1)
				}),
			),
		),
	)
	first := hsm.New(&groupBehaviorWorker{}, &workerModel, hsm.Config{ID: "first"})
	second := hsm.New(&groupBehaviorWorker{}, &workerModel, hsm.Config{ID: "second"})
	group := hsm.Start(context.Background(), hsm.MakeGroup("workers", first, second))
	controllerModel := hsm.Define(
		"GroupBehaviorControllerHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(event),
				hsm.Effect(group),
			),
		),
	)
	controller := hsm.Started(context.Background(), &THSM{}, &controllerModel)
	firstProcessed := hsm.AfterProcess(first.Context(), first, event)
	secondProcessed := hsm.AfterProcess(second.Context(), second, event)

	awaitWaiter(t, "controller dispatch", hsm.Dispatch(controller.Context(), controller, event))
	awaitWaiter(t, "first group member dispatch", firstProcessed)
	awaitWaiter(t, "second group member dispatch", secondProcessed)

	if got := first.seen.Load(); got != 1 {
		t.Fatalf("first group behavior count = %d, want 1", got)
	}
	if got := second.seen.Load(); got != 1 {
		t.Fatalf("second group behavior count = %d, want 1", got)
	}
}

func TestGroupCanBeCreatedOverStartedMembers(t *testing.T) {
	event := hsm.Event{Name: "Go"}
	model := hsm.Define(
		"StartedMemberGroupHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(hsm.On(event), hsm.Target("../done")),
		),
		hsm.State("done"),
	)
	first := hsm.Started(context.Background(), &THSM{}, &model, hsm.Config{ID: "first"})
	second := hsm.Started(first.Context(), &THSM{}, &model, hsm.Config{ID: "second"})
	group := hsm.MakeGroup("started", first, second)

	awaitWaiter(t, "started member group dispatch", hsm.Dispatch(first.Context(), group, event))
	if states := group.States(); !reflect.DeepEqual(states, []string{"/StartedMemberGroupHSM/done", "/StartedMemberGroupHSM/done"}) {
		t.Fatalf("started member group states = %#v", states)
	}
}

func TestTakeSnapshotCopiesMutableAttributeAndSchemaValues(t *testing.T) {
	schema := map[string]any{
		"owner":  "model",
		"nested": map[string]any{"value": "model"},
	}
	model := hsm.Define(
		"CanonicalSnapshotCopyHSM",
		hsm.Attribute("bag", map[string]any{
			"owner":  "runtime",
			"nested": []string{"runtime"},
		}),
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle"),
		hsm.Transition(
			hsm.On(hsm.Event{Name: "go", Kind: hsm.EventKind, Schema: schema}),
			hsm.Source("idle"),
			hsm.Target("idle"),
		),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)

	snapshot := hsm.TakeSnapshot(context.Background(), sm)
	attribute := snapshot.Attributes["/CanonicalSnapshotCopyHSM/bag"].(map[string]any)
	attribute["owner"] = "snapshot"
	attribute["nested"].([]string)[0] = "snapshot"
	eventSchema := snapshot.Events[0].Schema.(map[string]any)
	eventSchema["owner"] = "snapshot"
	eventSchema["nested"].(map[string]any)["value"] = "snapshot"

	value, ok := hsm.Get(context.Background(), sm, "bag")
	if !ok {
		t.Fatal("expected runtime attribute to exist")
	}
	runtimeAttribute := value.(map[string]any)
	if got := runtimeAttribute["owner"]; got != "runtime" {
		t.Fatalf("runtime attribute owner = %v, want runtime", got)
	}
	if got := runtimeAttribute["nested"].([]string)[0]; got != "runtime" {
		t.Fatalf("runtime nested attribute = %v, want runtime", got)
	}
	if got := schema["owner"]; got != "model" {
		t.Fatalf("model schema owner = %v, want model", got)
	}
	if got := schema["nested"].(map[string]any)["value"]; got != "model" {
		t.Fatalf("model nested schema = %v, want model", got)
	}

	fresh := hsm.TakeSnapshot(context.Background(), sm)
	freshAttribute := fresh.Attributes["/CanonicalSnapshotCopyHSM/bag"].(map[string]any)
	if got := freshAttribute["owner"]; got != "runtime" {
		t.Fatalf("fresh snapshot attribute owner = %v, want runtime", got)
	}
	if got := freshAttribute["nested"].([]string)[0]; got != "runtime" {
		t.Fatalf("fresh snapshot nested attribute = %v, want runtime", got)
	}
	freshSchema := fresh.Events[0].Schema.(map[string]any)
	if got := freshSchema["owner"]; got != "model" {
		t.Fatalf("fresh snapshot schema owner = %v, want model", got)
	}
	if got := freshSchema["nested"].(map[string]any)["value"]; got != "model" {
		t.Fatalf("fresh snapshot nested schema = %v, want model", got)
	}
}

func TestSnapshotClonesCyclicAttributeValues(t *testing.T) {
	type cyclicNode struct {
		Name string
		Next *cyclicNode
	}
	node := &cyclicNode{Name: "root"}
	node.Next = node
	model := hsm.Define(
		"CanonicalCyclicSnapshotHSM",
		hsm.Attribute("node", node),
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle"),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)

	snapshot := hsm.TakeSnapshot(context.Background(), sm)
	cloned := snapshot.Attributes["/CanonicalCyclicSnapshotHSM/node"].(*cyclicNode)
	if cloned == node {
		t.Fatal("snapshot cyclic node reused runtime pointer")
	}
	if cloned.Next != cloned {
		t.Fatalf("snapshot cyclic node did not preserve cycle: %#v -> %#v", cloned, cloned.Next)
	}
}
