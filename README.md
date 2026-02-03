# package hsm

`import "github.com/stateforward/hsm.go"`

Package hsm provides a powerful hierarchical state machine (HSM) implementation for Go.

# Overview

It enables modeling complex state-driven systems with features like hierarchical states,
entry/exit actions, guard conditions, and event-driven transitions. The implementation
follows the Unified DSK (Domain Specific Kit) specification, ensuring consistency across
platforms.

# Features

  - **Hierarchical States**: Support for nested states and regions.
  - **Event-Driven**: Asynchronous event processing with context propagation.
  - **Guards & Actions**: Flexible functional definitions for transition guards and state actions.
  - **Type Safe**: Generics-based implementation for state context.

# Usage

Define your state machine structure and behavior using the declarative builder pattern:

	type MyHSM struct {
	    hsm.HSM
	    counter int
	}

	model := hsm.Define(
	    "example",
	    hsm.State("foo"),
	    hsm.State("bar"),
	    hsm.Transition(
	        hsm.Trigger("moveToBar"),
	        hsm.Source("foo"),
	        hsm.Target("bar"),
	    ),
	    hsm.Initial("foo"),
	)

	// Start the state machine
	sm := hsm.Start(context.Background(), &MyHSM{}, &model)
	sm.Dispatch(hsm.Event{Name: "moveToBar"})

- `ErrNilHSM, ErrInvalidState, ErrMissingHSM, ErrMissingOperation, ErrInvalidOperation` — Error variables for common HSM error conditions.
- `InitialEvent, ErrorEvent, AnyEvent, FinalEvent, InfiniteDuration` — Built-in event types and special duration constants used by the HSM runtime.
- `Keys`
- `NullKind, ElementKind, NamespaceKind, VertexKind, ConstraintKind, BehaviorKind, ConcurrentKind, StateMachineKind, StateKind, RegionKind, TransitionKind, InternalKind, ExternalKind, LocalKind, SelfKind, EventKind, TimeEventKind, CompletionEventKind, ChangeEventKind, CallEventKind, ErrorEventKind, PseudostateKind, InitialKind, FinalStateKind, ChoiceKind, ShallowHistoryKind, DeepHistoryKind, CustomKind` — Kind constants define the HSM type hierarchy using bit-packed inheritance.
- `Version` — Version is the current semantic version of the hsm package.
- `func AfterDispatch(ctx context.Context, hsm Instance, event Event) <-chan struct{}` — AfterDispatch returns a channel that closes when the specified event is dispatched.
- `func AfterEntry(ctx context.Context, hsm Instance, state string) <-chan struct{}` — AfterEntry returns a channel that closes when the specified state is entered.
- `func AfterExecuted(ctx context.Context, hsm Instance, state string) <-chan struct{}` — AfterExecuted returns a channel that closes when the specified state's do-activity has completed execution.
- `func AfterExit(ctx context.Context, hsm Instance, state string) <-chan struct{}` — AfterExit returns a channel that closes when the specified state is exited.
- `func AfterProcess(ctx context.Context, hsm Instance, maybeEvent ...Event) <-chan struct{}` — AfterProcess returns a channel that closes when event processing completes.
- `func Call(ctx context.Context, hsm Instance, name string, args ...any) (any, error)` — Call dispatches an OnCall event and invokes the named operation.
- `func DispatchAll(ctx context.Context, event Event) <-chan struct{}` — DispatchAll sends an event to all state machine instances in the current context.
- `func DispatchTo(ctx context.Context, event Event, maybeIds ...string) <-chan struct{}`
- `func Dispatch[T context.Context](ctx T, hsm Instance, event Event) <-chan struct{}` — Dispatch sends an event to a specific state machine instance.
- `func Get(ctx context.Context, hsm Instance, name string) (any, bool)` — Get reads an attribute value from the given state machine or from context.
- `func ID(hsm Instance) string` — ID returns the unique identifier of a state machine instance.
- `func IsAncestor(current, target string) bool` — IsAncestor checks whether current is an ancestor of target in the state hierarchy.
- `func LCA(a, b string) string` — LCA finds the Lowest Common Ancestor between two qualified state names in a hierarchical state machine.
- `func Match(value string, patterns ...string) bool` — Match provides a simple interface, handling basic cases directly and delegating complex matching to the match function.
- `func Name(hsm Instance) string` — Name returns the simple name of a state machine instance (without path prefix).
- `func New[T Instance](sm T, model *Model, maybeConfig ...Config) T`
- `func QualifiedName(hsm Instance) string` — QualifiedName returns the fully qualified name of a state machine instance.
- `func Restart(ctx context.Context, hsm Instance, maybeData ...any) <-chan struct{}` — Restart stops a state machine and restarts it from the initial state.
- `func Set(ctx context.Context, hsm Instance, name string, value any) <-chan struct{}` — Set updates an attribute value and emits an OnSet change event.
- `func Start[T Instance](ctx context.Context, sm T, maybeData ...any) T`
- `func Started[T Instance](ctx context.Context, sm T, model *Model, maybeConfig ...Config) T` — Started creates and starts a new state machine instance with the given model and configuration.
- `func Stop(ctx context.Context, hsm Instance) <-chan struct{}` — Stop gracefully stops a state machine instance.
- `type AttributeChange` — AttributeChange is the payload for attribute change events.
- `type CallData` — CallData is the payload for call events.
- `type Config` — Config provides configuration options for state machine initialization.
- `type Element`
- `type EventDetail`
- `type Event`
- `type Expression` — Expression is a function type that evaluates a condition on a state machine.
- `type HSM`
- `type Instance` — Instance represents an active state machine instance that can process events and track state.
- `type Model` — Model represents the complete state machine model definition.
- `type Operation` — Operation is a function type that performs an action on a state machine.
- `type RedefinableElement` — RedefinableElement is a function type that modifies a Model by adding or updating elements.
- `type Snapshot`

