package hsm_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stateforward/hsm.go"
)

func TestRuntimeStopCancelsActivityAndClosesAfterExecuted(t *testing.T) {
	activityStarted := make(chan struct{})
	activityCancelled := make(chan struct{})

	model := hsm.Define(
		"RuntimeStopActivityHSM",
		hsm.Initial(hsm.Target("running")),
		hsm.State("running",
			hsm.Activity(func(ctx context.Context, sm *THSM, event hsm.Event) {
				close(activityStarted)
				<-ctx.Done()
				close(activityCancelled)
			}),
		),
	)

	sm := hsm.Started(context.Background(), &THSM{}, &model)
	awaitWaiter(t, "RuntimeStopActivityHSM activity start", activityStarted)

	executed := hsm.AfterExecuted(sm.Context(), sm, "/RuntimeStopActivityHSM/running")
	enteredAgain := hsm.AfterEntry(sm.Context(), sm, "/RuntimeStopActivityHSM/running")

	awaitWaiter(t, "RuntimeStopActivityHSM stop", hsm.Stop(context.Background(), sm))
	awaitWaiter(t, "RuntimeStopActivityHSM activity cancellation", activityCancelled)
	awaitWaiter(t, "RuntimeStopActivityHSM AfterExecuted waiter", executed)
	assertWaiterPending(t, "RuntimeStopActivityHSM re-entry after stop", enteredAgain)
}

func TestRuntimeDispatchIgnoresInactiveDirectInstance(t *testing.T) {
	advance := hsm.Event{Name: "advance"}
	handled := make(chan struct{}, 2)
	model := hsm.Define(
		"RuntimeInactiveDispatchHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(advance),
				hsm.Target("../done"),
				hsm.Effect(func(ctx context.Context, sm *THSM, event hsm.Event) {
					handled <- struct{}{}
				}),
			),
		),
		hsm.State("done"),
	)

	sm := hsm.New(&THSM{}, &model)
	assertCompletionErr(t, "unstarted direct dispatch", hsm.Dispatch(context.Background(), sm, advance), hsm.ErrInvalidState)
	select {
	case <-handled:
		t.Fatal("unstarted direct dispatch executed transition effect")
	case <-time.After(waiterShouldRemainPendingFor):
	}
	if got := sm.State(); got != "" {
		t.Fatalf("unstarted state = %q, want empty state", got)
	}

	hsm.Start(context.Background(), sm)
	awaitWaiter(t, "stop inactive dispatch hsm", hsm.Stop(context.Background(), sm))
	assertCompletionErr(t, "stopped direct dispatch", hsm.Dispatch(context.Background(), sm, advance), hsm.ErrInvalidState)
	select {
	case <-handled:
		t.Fatal("stopped direct dispatch executed transition effect")
	case <-time.After(waiterShouldRemainPendingFor):
	}
}

func TestRuntimeCanceledStopDoesNotPoisonProcessingLock(t *testing.T) {
	advance := hsm.Event{Name: "advance"}
	model := hsm.Define(
		"RuntimeCanceledStopHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(advance),
				hsm.Target("../done"),
			),
		),
		hsm.State("done"),
	)

	sm := hsm.Started(context.Background(), &THSM{}, &model)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	awaitWaiter(t, "canceled stop returns", hsm.Stop(ctx, sm))
	awaitWaiter(t, "dispatch after canceled stop", hsm.Dispatch(context.Background(), sm, advance))
	if sm.State() != "/RuntimeCanceledStopHSM/done" {
		t.Fatalf("state after canceled stop dispatch = %s", sm.State())
	}
}

func TestRuntimeRestartWithOwnContextDetachesFromCanceledRuntimeContext(t *testing.T) {
	advance := hsm.Event{Name: "advance"}
	model := hsm.Define(
		"RuntimeOwnContextRestartHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(advance),
				hsm.Target("../done"),
			),
		),
		hsm.State("done"),
	)

	sm := hsm.Started(context.Background(), &THSM{}, &model, hsm.Config{ID: "own-context-restart"})
	awaitWaiter(t, "advance before own-context restart", hsm.Dispatch(context.Background(), sm, advance))
	if sm.State() != "/RuntimeOwnContextRestartHSM/done" {
		t.Fatalf("state before own-context restart = %s", sm.State())
	}

	awaitWaiter(t, "own-context restart", hsm.Restart(sm.Context(), sm))
	if sm.State() != "/RuntimeOwnContextRestartHSM/idle" {
		t.Fatalf("state after own-context restart = %s, want idle", sm.State())
	}
	if current, ok := hsm.FromContext(sm.Context()); !ok || hsm.ID(current) != "own-context-restart" {
		t.Fatalf("FromContext(sm.Context()) after restart = (%v, %v), want restarted instance", current, ok)
	}
	instances, ok := hsm.InstancesFromContext(sm.Context())
	if !ok || len(instances) != 1 || hsm.ID(instances[0]) != "own-context-restart" {
		t.Fatalf("InstancesFromContext(sm.Context()) after restart = (%v, %v), want restarted instance", instances, ok)
	}

	awaitWaiter(t, "dispatch after own-context restart", hsm.Dispatch(context.Background(), sm, advance))
	if sm.State() != "/RuntimeOwnContextRestartHSM/done" {
		t.Fatalf("state after dispatch following own-context restart = %s", sm.State())
	}
}

func TestRuntimeRestartChildWithOwnContextPreservesParentOwner(t *testing.T) {
	advance := hsm.Event{Name: "advance"}
	parentModel := hsm.Define(
		"RuntimeRestartParentHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle"),
	)
	childModel := hsm.Define(
		"RuntimeRestartChildHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(advance),
				hsm.Target("../done"),
			),
		),
		hsm.State("done"),
	)

	parent := hsm.Started(context.Background(), &THSM{}, &parentModel, hsm.Config{ID: "parent"})
	child := hsm.Started(parent.Context(), &THSM{}, &childModel, hsm.Config{ID: "child"})

	awaitWaiter(t, "child advance before own-context restart", hsm.Dispatch(context.Background(), child, advance))
	if child.State() != "/RuntimeRestartChildHSM/done" {
		t.Fatalf("child state before own-context restart = %s", child.State())
	}

	awaitWaiter(t, "child own-context restart", hsm.Restart(child.Context(), child))
	if child.State() != "/RuntimeRestartChildHSM/idle" {
		t.Fatalf("child state after own-context restart = %s, want idle", child.State())
	}
	if owner, ok := child.Context().Value(hsm.Keys.Owner).(hsm.Instance); !ok || hsm.ID(owner) != "parent" {
		t.Fatalf("child owner after own-context restart = (%v, %v), want parent", owner, ok)
	}
	instances, ok := hsm.InstancesFromContext(child.Context())
	if !ok || len(instances) != 2 {
		t.Fatalf("InstancesFromContext(child.Context()) after restart = (%v, %v), want parent and child", instances, ok)
	}

	awaitWaiter(t, "dispatch all after child own-context restart", hsm.DispatchAll(child.Context(), advance))
	if child.State() != "/RuntimeRestartChildHSM/done" {
		t.Fatalf("child state after dispatch all following own-context restart = %s", child.State())
	}
	if parent.State() != "/RuntimeRestartParentHSM/idle" {
		t.Fatalf("parent state after child own-context restart = %s, want idle", parent.State())
	}
}

func TestRuntimeCanceledRestartDoesNotPoisonMachine(t *testing.T) {
	advance := hsm.Event{Name: "advance"}
	model := hsm.Define(
		"RuntimeCanceledRestartHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(advance),
				hsm.Target("../done"),
			),
		),
		hsm.State("done"),
	)

	sm := hsm.Started(context.Background(), &THSM{}, &model)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertWaiterClosed(t, "canceled restart returns", hsm.Restart(ctx, sm))
	if sm.State() != "/RuntimeCanceledRestartHSM/idle" {
		t.Fatalf("state after canceled restart = %s, want idle", sm.State())
	}

	awaitWaiter(t, "dispatch after canceled restart", hsm.Dispatch(context.Background(), sm, advance))
	if sm.State() != "/RuntimeCanceledRestartHSM/done" {
		t.Fatalf("state after dispatch following canceled restart = %s", sm.State())
	}
}

func TestStartupErrorDoesNotPoisonProcessingLock(t *testing.T) {
	afterStartup := hsm.Event{Name: "after_startup"}
	model := hsm.Define(
		"StartupErrorLifecycleHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Entry(func(context.Context, *THSM, hsm.Event) {
				panic("startup entry boom")
			}),
		),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	if sm.State() != "/StartupErrorLifecycleHSM" {
		t.Fatalf("startup error state = %s, want root state", sm.State())
	}

	awaitWaiter(t, "dispatch after startup error", hsm.Dispatch(context.Background(), sm, afterStartup))
}

func TestRuntimeLifecycleGuardsNativeAPIs(t *testing.T) {
	setEvent := hsm.Event{Name: "/RuntimeLifecycleNativeGuardsHSM/flag", Kind: hsm.ChangeEventKind}
	goEvent := hsm.Event{Name: "go"}
	calls := 0
	effects := make(chan string, 4)

	model := hsm.Define(
		"RuntimeLifecycleNativeGuardsHSM",
		hsm.Attribute("flag", false),
		hsm.Operation("audit", func(context.Context, *THSM) {
			calls++
		}),
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(setEvent),
				hsm.Target("../set_seen"),
				hsm.Effect(func(context.Context, *THSM, hsm.Event) {
					effects <- "set"
				}),
			),
			hsm.Transition(
				hsm.On(goEvent),
				hsm.Target("../done"),
				hsm.Effect(func(context.Context, *THSM, hsm.Event) {
					effects <- "go"
				}),
			),
		),
		hsm.State("set_seen"),
		hsm.State("done"),
	)

	sm := hsm.New(&THSM{}, &model)
	assertCompletionErr(t, "set before start", hsm.Set(context.Background(), sm, "flag", true), hsm.ErrInvalidState)
	if got, _ := hsm.Get(context.Background(), sm, "flag"); got != false {
		t.Fatalf("set before start mutated flag to %#v", got)
	}
	if _, err := hsm.Call(context.Background(), sm, "audit"); !errors.Is(err, hsm.ErrInvalidState) {
		t.Fatalf("call before start error = %v, want ErrInvalidState", err)
	}
	if calls != 0 {
		t.Fatalf("call before start invoked operation %d time(s)", calls)
	}
	assertCompletionErr(t, "restart before start", hsm.Restart(context.Background(), sm), hsm.ErrInvalidState)
	if sm.State() != "" {
		t.Fatalf("restart before start changed state to %s", sm.State())
	}
	beforeSnapshot := hsm.TakeSnapshot(context.Background(), sm)
	if beforeSnapshot.State != "/RuntimeLifecycleNativeGuardsHSM" || len(beforeSnapshot.Attributes) != 0 {
		t.Fatalf("snapshot before start = state %q attrs %#v", beforeSnapshot.State, beforeSnapshot.Attributes)
	}

	hsm.Start(context.Background(), sm)
	assertPanicContains(t, "double start", hsm.ErrAlreadyStarted.Error(), func() {
		hsm.Start(context.Background(), sm)
	})
	awaitWaiter(t, "stop lifecycle guard hsm", hsm.Stop(context.Background(), sm))

	assertCompletionErr(t, "dispatch after stop", hsm.Dispatch(context.Background(), sm, goEvent), hsm.ErrInvalidState)
	assertCompletionErr(t, "set after stop", hsm.Set(context.Background(), sm, "flag", true), hsm.ErrInvalidState)
	if got, _ := hsm.Get(context.Background(), sm, "flag"); got != false {
		t.Fatalf("set after stop mutated flag to %#v", got)
	}
	if _, err := hsm.Call(context.Background(), sm, "audit"); !errors.Is(err, hsm.ErrInvalidState) {
		t.Fatalf("call after stop error = %v, want ErrInvalidState", err)
	}
	if calls != 0 {
		t.Fatalf("call after stop invoked operation %d time(s)", calls)
	}
	assertCompletionErr(t, "restart after stop", hsm.Restart(context.Background(), sm), hsm.ErrInvalidState)
	afterSnapshot := hsm.TakeSnapshot(context.Background(), sm)
	if afterSnapshot.State != "/RuntimeLifecycleNativeGuardsHSM" || len(afterSnapshot.Attributes) != 0 {
		t.Fatalf("snapshot after stop = state %q attrs %#v", afterSnapshot.State, afterSnapshot.Attributes)
	}
	select {
	case effect := <-effects:
		t.Fatalf("inactive operation executed %s effect", effect)
	case <-time.After(waiterShouldRemainPendingFor):
	}
}

func TestRuntimeStopDoesNotProcessQueuedRegularEventsAfterCancellation(t *testing.T) {
	var mutex sync.Mutex
	events := []hsm.Event{}
	allowPop := false
	handled := make(chan struct{}, 1)

	queue := hsm.Queue{
		Push: func(_ context.Context, event hsm.Event) error {
			mutex.Lock()
			defer mutex.Unlock()
			events = append(events, event)
			return nil
		},
		Pop: func(context.Context) (hsm.Event, bool, error) {
			mutex.Lock()
			defer mutex.Unlock()
			if !allowPop || len(events) == 0 {
				return hsm.Event{}, false, nil
			}
			event := events[0]
			events = events[1:]
			return event, true, nil
		},
		Len: func(context.Context) (int, error) {
			mutex.Lock()
			defer mutex.Unlock()
			return len(events), nil
		},
	}

	model := hsm.Define(
		"RuntimeStopQueuedRegularEventHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle"),
		hsm.Transition(
			hsm.On("go"),
			hsm.Effect(func(context.Context, *THSM, hsm.Event) {
				handled <- struct{}{}
			}),
		),
	)

	sm := hsm.Started(context.Background(), &THSM{}, &model, hsm.Config{Queue: queue})
	awaitWaiter(t, "queued regular dispatch", hsm.Dispatch(sm.Context(), sm, hsm.Event{Name: "go"}))
	if sm.State() != "/RuntimeStopQueuedRegularEventHSM/idle" {
		t.Fatalf("expected queued event to remain unprocessed before stop, got %s", sm.State())
	}

	mutex.Lock()
	allowPop = true
	mutex.Unlock()
	awaitWaiter(t, "stop with queued regular event", hsm.Stop(context.Background(), sm))

	select {
	case <-handled:
		t.Fatal("queued regular event executed after stop")
	case <-time.After(waiterShouldRemainPendingFor):
	}
}

func TestRuntimeRestartReentersInitialStateWithData(t *testing.T) {
	advanceEvent := hsm.Event{Name: "advance"}
	initialData := make(chan any, 2)

	model := hsm.Define(
		"RuntimeRestartLifecycleHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Entry(func(ctx context.Context, sm *THSM, event hsm.Event) {
				initialData <- event.Data
			}),
		),
		hsm.Transition(hsm.On(advanceEvent), hsm.Source("idle"), hsm.Target("done")),
		hsm.State("done"),
	)

	sm := hsm.Started(context.Background(), &THSM{}, &model, hsm.Config{Data: "boot"})

	select {
	case got := <-initialData:
		if got != "boot" {
			t.Fatalf("expected initial start data %q, got %#v", "boot", got)
		}
	case <-time.After(waiterDeadline):
		t.Fatal("timed out waiting for initial start data")
	}

	awaitWaiter(t, "RuntimeRestartLifecycleHSM advance dispatch", hsm.Dispatch(sm.Context(), sm, advanceEvent))
	if sm.State() != "/RuntimeRestartLifecycleHSM/done" {
		t.Fatalf("expected state to be done before restart, got %s", sm.State())
	}

	reenteredIdle := hsm.AfterEntry(sm.Context(), sm, "/RuntimeRestartLifecycleHSM/idle")
	exitedDone := hsm.AfterExit(sm.Context(), sm, "/RuntimeRestartLifecycleHSM/done")
	awaitWaiter(t, "RuntimeRestartLifecycleHSM restart", hsm.Restart(context.Background(), sm, "again"))
	awaitWaiter(t, "RuntimeRestartLifecycleHSM done exit on restart", exitedDone)
	awaitWaiter(t, "RuntimeRestartLifecycleHSM idle re-entry on restart", reenteredIdle)

	if sm.State() != "/RuntimeRestartLifecycleHSM/idle" {
		t.Fatalf("expected state to reset to idle after restart, got %s", sm.State())
	}

	select {
	case got := <-initialData:
		if got != "again" {
			t.Fatalf("expected restart data %q, got %#v", "again", got)
		}
	case <-time.After(waiterDeadline):
		t.Fatal("timed out waiting for restart data")
	}
}

func TestRuntimeStopTimeoutDispatchesErrorEventDeterministically(t *testing.T) {
	harness := newDeterministicClockHarness()
	activityStarted := make(chan struct{})
	releaseActivity := make(chan struct{})
	errorsSeen := make(chan error, 1)

	t.Cleanup(func() {
		close(releaseActivity)
	})

	model := hsm.Define(
		"RuntimeStopTimeoutHSM",
		hsm.Initial(hsm.Target("running")),
		hsm.State("running",
			hsm.Activity(func(ctx context.Context, sm *THSM, event hsm.Event) {
				close(activityStarted)
				<-releaseActivity
			}),
		),
		hsm.Transition(
			hsm.On(hsm.ErrorEvent),
			hsm.Effect(func(ctx context.Context, sm *THSM, event hsm.Event) {
				err, ok := event.Data.(error)
				if !ok {
					return
				}
				select {
				case errorsSeen <- err:
				default:
				}
			}),
		),
	)

	sm := hsm.Started(
		context.Background(),
		&THSM{},
		&model,
		hsm.Config{
			Clock:           harness.Clock(),
			ActivityTimeout: 5 * time.Second,
		},
	)
	awaitWaiter(t, "RuntimeStopTimeoutHSM activity start", activityStarted)

	errorProcessed := hsm.AfterProcess(sm.Context(), sm, hsm.ErrorEvent)
	stopCompleted := hsm.Stop(context.Background(), sm)

	registration := harness.awaitRegistration(t, "RuntimeStopTimeoutHSM termination timeout")
	if registration.kind != "after" {
		t.Fatalf("expected terminate timeout to use clock.After, got %s", registration.kind)
	}
	if registration.requested != 5*time.Second {
		t.Fatalf("expected terminate timeout duration %v, got %v", 5*time.Second, registration.requested)
	}

	assertWaiterPending(t, "RuntimeStopTimeoutHSM stop before timeout", stopCompleted)
	registration.trigger(t)
	awaitWaiter(t, "RuntimeStopTimeoutHSM stop after timeout", stopCompleted)
	awaitWaiter(t, "RuntimeStopTimeoutHSM error processing", errorProcessed)

	select {
	case err := <-errorsSeen:
		if !strings.Contains(err.Error(), "terminate timeout: /RuntimeStopTimeoutHSM/running/") {
			t.Fatalf("expected terminate timeout error for running activity, got %v", err)
		}
	case <-time.After(waiterDeadline):
		t.Fatal("timed out waiting for termination timeout error")
	}
}
