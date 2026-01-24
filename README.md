# package hsm

`import "github.com/stateforward/hsm-go"`

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

- `ErrNilHSM, ErrInvalidState, ErrMissingHSM`
- `InitialEvent, ErrorEvent, AnyEvent, FinalEvent, InfiniteDuration`
- `Keys`
- `NullKind, ElementKind, NamespaceKind, VertexKind, ConstraintKind, BehaviorKind, ConcurrentKind, StateMachineKind, StateKind, RegionKind, TransitionKind, InternalKind, ExternalKind, LocalKind, SelfKind, EventKind, TimeEventKind, CompletionEventKind, ErrorEventKind, PseudostateKind, InitialKind, FinalStateKind, ChoiceKind, CustomKind`
- `closedChannel`
- `empty`
- `func AfterDispatch(ctx context.Context, hsm Instance, event Event) <-chan struct{}`
- `func AfterEntry(ctx context.Context, hsm Instance, state string) <-chan struct{}`
- `func AfterExecuted(ctx context.Context, hsm Instance, state string) <-chan struct{}`
- `func AfterExit(ctx context.Context, hsm Instance, state string) <-chan struct{}`
- `func AfterProcess(ctx context.Context, hsm Instance, maybeEvent ...Event) <-chan struct{}`
- `func DispatchAll(ctx context.Context, event Event) <-chan struct{}` — DispatchAll sends an event to all state machine instances in the current context.
- `func DispatchTo(ctx context.Context, event Event, maybeIds ...string) <-chan struct{}`
- `func Dispatch[T context.Context](ctx T, hsm Instance, event Event) <-chan struct{}` — Dispatch sends an event to a specific state machine instance.
- `func ID(hsm Instance) string`
- `func IsAncestor(current, target string) bool`
- `func LCA(a, b string) string` — LCA finds the Lowest Common Ancestor between two qualified state names in a hierarchical state machine.
- `func Match(value string, patterns ...string) bool` — Match provides a simple interface, handling basic cases directly and delegating complex matching to the match function.
- `func Name(hsm Instance) string`
- `func New[T Instance](sm T, model *Model, maybeConfig ...Config) T`
- `func QualifiedName(hsm Instance) string`
- `func Restart(ctx context.Context, hsm Instance, maybeData ...any) <-chan struct{}`
- `func Start[T Instance](ctx context.Context, sm T, maybeData ...any) T`
- `func Started[T Instance](ctx context.Context, sm T, model *Model, maybeConfig ...Config) T` — Started creates and starts a new state machine instance with the given model and configuration.
- `func Stop(ctx context.Context, hsm Instance) <-chan struct{}` — Stop gracefully stops a state machine instance.
- `func apply(model *Model, stack []Element, partials ...RedefinableElement)`
- `func buildCaches(model *Model)`
- `func cleanup[T Instance](ctx context.Context, sm *hsm[T], element Element)`
- `func getFunctionName(fn any) string`
- `func get[T Element](model *Model, name string) T`
- `func parse(value, pattern string) bool` — parse implements wildcard matching using a goto-based iterative approach.
- `func traceback(maybeError ...error) func(err error)`
- `type Config` — Config provides configuration options for state machine initialization.
- `type Element`
- `type EventDetail`
- `type Event`
- `type Expression`
- `type HSM`
- `type Instance` — Instance represents an active state machine instance that can process events and track state.
- `type Model` — Model represents the complete state machine model definition.
- `type Operation`
- `type RedefinableElement` — RedefinableElement is a function type that modifies a Model by adding or updating elements.
- `type Snapshot`
- `type active`
- `type after`
- `type behavior`
- `type constraint`
- `type ctx`
- `type element`
- `type hsm`
- `type instance`
- `type key`
- `type mutex`
- `type paths`
- `type queue`
- `type state`
- `type timeouts`
- `type transition`
- `type vertex`

### Variables

#### NullKind, ElementKind, NamespaceKind, VertexKind, ConstraintKind, BehaviorKind, ConcurrentKind, StateMachineKind, StateKind, RegionKind, TransitionKind, InternalKind, ExternalKind, LocalKind, SelfKind, EventKind, TimeEventKind, CompletionEventKind, ErrorEventKind, PseudostateKind, InitialKind, FinalStateKind, ChoiceKind, CustomKind

```go
var (
	NullKind            = kind.Make()
	ElementKind         = kind.Make()
	NamespaceKind       = kind.Make(ElementKind)
	VertexKind          = kind.Make(ElementKind)
	ConstraintKind      = kind.Make(ElementKind)
	BehaviorKind        = kind.Make(ElementKind)
	ConcurrentKind      = kind.Make(BehaviorKind)
	StateMachineKind    = kind.Make(ConcurrentKind, NamespaceKind)
	StateKind           = kind.Make(VertexKind, NamespaceKind)
	RegionKind          = kind.Make(ElementKind)
	TransitionKind      = kind.Make(ElementKind)
	InternalKind        = kind.Make(TransitionKind)
	ExternalKind        = kind.Make(TransitionKind)
	LocalKind           = kind.Make(TransitionKind)
	SelfKind            = kind.Make(TransitionKind)
	EventKind           = kind.Make(ElementKind)
	TimeEventKind       = kind.Make(EventKind)
	CompletionEventKind = kind.Make(EventKind)
	ErrorEventKind      = kind.Make(CompletionEventKind)
	PseudostateKind     = kind.Make(VertexKind)
	InitialKind         = kind.Make(PseudostateKind)
	FinalStateKind      = kind.Make(StateKind)
	ChoiceKind          = kind.Make(PseudostateKind)
	CustomKind          = kind.Make(ElementKind)
)
```

#### ErrNilHSM, ErrInvalidState, ErrMissingHSM

```go
var (
	ErrNilHSM       = errors.New("hsm is nil")
	ErrInvalidState = errors.New("invalid state")
	ErrMissingHSM   = errors.New("missing hsm in context")
)
```

#### InitialEvent, ErrorEvent, AnyEvent, FinalEvent, InfiniteDuration

```go
var (
	InitialEvent = Event{
		Name: "hsm_initial",
		Kind: CompletionEventKind,
	}
	ErrorEvent = Event{
		Name: "hsm_error",
		Kind: ErrorEventKind,
	}
	AnyEvent = Event{
		Name: "*",
		Kind: EventKind,
	}
	FinalEvent = Event{
		Name: "hsm_final",
		Kind: CompletionEventKind,
	}
	InfiniteDuration = time.Duration(-1)
)
```

#### Keys

```go
var Keys = struct {
	Instances key[*atomic.Pointer[[]Instance]]
	Owner     key[Instance]
	HSM       key[HSM]
}{
	Instances: key[*atomic.Pointer[[]Instance]]{},
	Owner:     key[Instance]{},
	HSM:       key[HSM]{},
}
```

#### closedChannel

```go
var closedChannel = func() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()
```

#### empty

```go
var empty = Event{}
```


### Functions

#### AfterDispatch

```go
func AfterDispatch(ctx context.Context, hsm Instance, event Event) <-chan struct{}
```

#### AfterEntry

```go
func AfterEntry(ctx context.Context, hsm Instance, state string) <-chan struct{}
```

#### AfterExecuted

```go
func AfterExecuted(ctx context.Context, hsm Instance, state string) <-chan struct{}
```

#### AfterExit

```go
func AfterExit(ctx context.Context, hsm Instance, state string) <-chan struct{}
```

#### AfterProcess

```go
func AfterProcess(ctx context.Context, hsm Instance, maybeEvent ...Event) <-chan struct{}
```

#### Dispatch

```go
func Dispatch[T context.Context](ctx T, hsm Instance, event Event) <-chan struct{}
```

Dispatch sends an event to a specific state machine instance.
Returns a channel that closes when the event has been fully processed.

Example:

	sm := hsm.Start(...)
	done := sm.Dispatch(hsm.Event{Name: "start"})
	<-done // Wait for event processing to complete

#### DispatchAll

```go
func DispatchAll(ctx context.Context, event Event) <-chan struct{}
```

DispatchAll sends an event to all state machine instances in the current context.
Returns a channel that closes when all instances have processed the event.
DispatchAll sends an event to all state machine instances in the current context.
Returns a channel that closes when all instances have processed the event.

Example:

	sm1 := hsm.Start(...)
	sm2 := hsm.Start(...)
	done := hsm.DispatchAll(context.Background(), hsm.Event{Name: "globalEvent"})
	<-done // Wait for all instances to process the event

#### DispatchTo

```go
func DispatchTo(ctx context.Context, event Event, maybeIds ...string) <-chan struct{}
```

#### ID

```go
func ID(hsm Instance) string
```

#### IsAncestor

```go
func IsAncestor(current, target string) bool
```

#### LCA

```go
func LCA(a, b string) string
```

LCA finds the Lowest Common Ancestor between two qualified state names in a hierarchical state machine.
It takes two qualified names 'a' and 'b' as strings and returns their closest common ancestor.

For example:
- LCA("/s/s1", "/s/s2") returns "/s"
- LCA("/s/s1", "/s/s1/s11") returns "/s/s1"
- LCA("/s/s1", "/s/s1") returns "/s/s1"

#### Match

```go
func Match(value string, patterns ...string) bool
```

Match provides a simple interface, handling basic cases directly
and delegating complex matching to the match function.

#### Name

```go
func Name(hsm Instance) string
```

#### New

```go
func New[T Instance](sm T, model *Model, maybeConfig ...Config) T
```

#### QualifiedName

```go
func QualifiedName(hsm Instance) string
```

#### Restart

```go
func Restart(ctx context.Context, hsm Instance, maybeData ...any) <-chan struct{}
```

#### Start

```go
func Start[T Instance](ctx context.Context, sm T, maybeData ...any) T
```

#### Started

```go
func Started[T Instance](ctx context.Context, sm T, model *Model, maybeConfig ...Config) T
```

Started creates and starts a new state machine instance with the given model and configuration.
The state machine will begin executing from its initial state.

Example:

	model := hsm.Define(...)
	sm := hsm.Started(context.Background(), &MyHSM{}, &model, hsm.Config{
	    Trace: func(ctx context.Context, step string, data ...any) (context.Context, func(...any)) {
	        log.Printf("Step: %s, Data: %v", step, data)
	        return ctx, func(...any) {}
	    },
	    Id: "my-hsm-1",
	})

#### Stop

```go
func Stop(ctx context.Context, hsm Instance) <-chan struct{}
```

Stop gracefully stops a state machine instance.
It cancels any running activities and prevents further event processing.

Example:

	sm := hsm.Start(...)
	// ... use state machine ...
	hsm.Stop(sm)

#### apply

```go
func apply(model *Model, stack []Element, partials ...RedefinableElement)
```

#### buildCaches

```go
func buildCaches(model *Model)
```

#### cleanup

```go
func cleanup[T Instance](ctx context.Context, sm *hsm[T], element Element)
```

#### get

```go
func get[T Element](model *Model, name string) T
```

#### getFunctionName

```go
func getFunctionName(fn any) string
```

#### parse

```go
func parse(value, pattern string) bool
```

parse implements wildcard matching using a goto-based iterative approach.
It supports the '*' wildcard, which matches zero or more characters.

#### traceback

```go
func traceback(maybeError ...error) func(err error)
```


## type Config

```go
type Config struct {
	// ID is a unique identifier for the state machine instance.
	ID string
	// ActivityTimeout is the timeout for the state activity to terminate.
	ActivityTimeout time.Duration
	// Name is the name of the state machine.
	Name string
	// Data to be passed during initialization
	Data any
}
```

Config provides configuration options for state machine initialization.

## type Element

```go
type Element interface {
	Id() string
	Kind() uint64
	Owner() string
	QualifiedName() string
	Name() string
}
```

### Functions returning Element

#### find

```go
func find(stack []Element, maybeKinds ...uint64) Element
```


## type Event

```go
type Event struct {
	Kind   uint64 `xml:"kind,attr" json:"kind"`
	Name   string `xml:"name,attr" json:"name"`
	ID     string `xml:"id,attr" json:"id"`
	Source string `xml:"source,attr,omitempty" json:"source,omitempty"`
	Target string `xml:"target,attr,omitempty" json:"target,omitempty"`
	Data   any    `xml:"data" json:"data"`
	Schema any    `xml:"schema" json:"schema"`
}
```

### Methods

#### Event.WithData

```go
func () WithData(data any) Event
```

#### Event.WithDataAndID

```go
func () WithDataAndID(data any, id string) Event
```


## type EventDetail

```go
type EventDetail struct {
	Event  string
	Target string
	Guard  bool
	Schema any
}
```

## type Expression

```go
type Expression[T Instance] func(ctx context.Context, hsm T, event Event) bool
```

## type HSM

```go
type HSM struct {
	instance
}
```

### Methods

#### HSM.bind

```go
func () bind(instance Instance)
```


## type Instance

```go
type Instance interface {
	// State returns the current state's qualified name.
	State() string
	Context() context.Context
	// non exported
	channels() *after
	takeSnapshot() Snapshot
	wait() <-chan struct{}
	start(ctx context.Context, instance Instance, event *Event)
	dispatch(ctx context.Context, event Event) <-chan struct{}
	bind(instance Instance)
	stop(ctx context.Context) <-chan struct{}
	restart(ctx context.Context, maybeData ...any) <-chan struct{}
}
```

Instance represents an active state machine instance that can process events and track state.
It provides methods for event dispatch and state management.

### Functions returning Instance

#### FromContext

```go
func FromContext(ctx context.Context) (Instance, bool)
```

FromContext retrieves a state machine instance from a context.
Returns the instance and a boolean indicating whether it was found.

Example:

	if sm, ok := hsm.FromContext(ctx); ok {
	    log.Printf("Current state: %s", sm.State())
	}

#### InstancesFromContext

```go
func InstancesFromContext(ctx context.Context) ([]Instance, bool)
```


## type Model

```go
type Model struct {
	state
	members  map[string]Element
	events   map[string]*Event
	elements []RedefinableElement
	// TransitionMap provides fast lookup of transitions by state and event name
	TransitionMap map[string]map[string][]*transition // stateQualifiedName -> eventName -> transitions
	// DeferredMap provides fast lookup of deferred events by state
	DeferredMap map[string]map[string]struct{} // stateQualifiedName -> Set<deferredEventNames>
}
```

Model represents the complete state machine model definition.
It contains the root state and maintains a namespace of all elements.

### Functions returning Model

#### Define

```go
func Define(name string, redefinableElements ...RedefinableElement) Model
```

Define creates a new state machine model with the given name and elements.
The first argument can be either a string name or a RedefinableElement.
Additional elements are added to the model in the order they are specified.

Example:

	model := hsm.Define(
	    "traffic_light",
	    hsm.State("red"),
	    hsm.State("yellow"),
	    hsm.State("green"),
	    hsm.Initial("red")
	)


### Methods

#### Model.Activities

```go
func () Activities() []string
```

#### Model.Entry

```go
func () Entry() []string
```

#### Model.Exit

```go
func () Exit() []string
```

#### Model.Id

```go
func () Id() string
```

#### Model.Kind

```go
func () Kind() uint64
```

#### Model.Members

```go
func () Members() map[string]Element
```

#### Model.Name

```go
func () Name() string
```

#### Model.Owner

```go
func () Owner() string
```

#### Model.QualifiedName

```go
func () QualifiedName() string
```

#### Model.Transitions

```go
func () Transitions() []string
```

#### Model.push

```go
func () push(partial RedefinableElement)
```


## type Operation

```go
type Operation[T Instance] func(ctx context.Context, hsm T, event Event)
```

## type RedefinableElement

```go
type RedefinableElement = func(model *Model, stack []Element) Element
```

RedefinableElement is a function type that modifies a Model by adding or updating elements.
It's used to build the state machine structure in a declarative way.

### Functions returning RedefinableElement

#### Activity

```go
func Activity[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement
```

Activity defines a long-running action that is executed while in a state.
The activity is started after the entry action and stopped before the exit action.

Example:

	hsm.Activity(func(ctx context.Context, hsm *MyHSM, event Event) {
	    for {
	        select {
	        case <-ctx.Done():
	            return
	        case <-time.After(time.Second):
	            log.Println("Activity tick")
	        }
	    }
	})

#### After

```go
func After[T Instance](expr func(ctx context.Context, hsm T, event Event) time.Duration) RedefinableElement
```

After creates a time-based transition that occurs after a specified duration.
The duration can be dynamically computed based on the state machine's context.

Example:

	hsm.Transition(
	    hsm.After(func(ctx context.Context, hsm *MyHSM, event Event) time.Duration {
	        return time.Second * 30
	    }),
	    hsm.Source("active"),
	    hsm.Target("timeout")
	)

#### Choice

```go
func Choice[T interface{ RedefinableElement | string }](elementOrName T, partialElements ...RedefinableElement) RedefinableElement
```

Choice creates a pseudo-state that enables dynamic branching based on guard conditions.
The first transition with a satisfied guard condition is taken.

Example:

	hsm.Choice(
	    hsm.Transition(
	        hsm.Target("approved"),
	        hsm.Guard(func(ctx context.Context, hsm *MyHSM, event Event) bool {
	            return hsm.score > 700
	        })
	    ),
	    hsm.Transition(
	        hsm.Target("rejected")
	    )
	)

#### Defer

```go
func Defer[T interface {
	string | *Event | Event
}](events ...T) RedefinableElement
```

Defer schedules events to be processed after the current state is exited.

Example:

	hsm.Defer(hsm.Event{Name: "event_name"})

#### Effect

```go
func Effect[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement
```

Effect defines an action to be executed during a transition.
The effect function is called after exiting the source state and before entering the target state.

Example:

	hsm.Effect(func(ctx context.Context, hsm *MyHSM, event Event) {
	    log.Printf("Transitioning with event: %s", event.Name)
	})

#### Entry

```go
func Entry[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement
```

Entry defines an action to be executed when entering a state.
The entry action is executed before any internal activities are started.

Example:

	hsm.Entry(func(ctx context.Context, hsm *MyHSM, event Event) {
	    log.Printf("Entering state with event: %s", event.Name)
	})

#### Every

```go
func Every[T Instance](expr func(ctx context.Context, hsm T, event Event) time.Duration) RedefinableElement
```

Every schedules events to be processed on an interval.

Example:

	hsm.Every(func(ctx context.Context, hsm T, event Event) time.Duration {
	    return time.Second * 30
	})

#### Exit

```go
func Exit[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement
```

Exit defines an action to be executed when exiting a state.
The exit action is executed after any internal activities are stopped.

Example:

	hsm.Exit(func(ctx context.Context, hsm *MyHSM, event Event) {
	    log.Printf("Exiting state with event: %s", event.Name)
	})

#### Final

```go
func Final(name string) RedefinableElement
```

Final creates a final state that represents the completion of a composite state or the entire state machine.
When a final state is entered, a completion event is generated.

Example:

	hsm.State("process",
	    hsm.State("working"),
	    hsm.Final("done"),
	    hsm.Transition(
	        hsm.Source("working"),
	        hsm.Target("done")
	    )
	)

#### Guard

```go
func Guard[T Instance](fn func(ctx context.Context, hsm T, event Event) bool) RedefinableElement
```

Guard defines a condition that must be true for a transition to be taken.
If multiple transitions are possible, the first one with a satisfied guard is chosen.

Example:

	hsm.Guard(func(ctx context.Context, hsm *MyHSM, event Event) bool {
	    return hsm.counter > 10
	})

#### Initial

```go
func Initial(partialElements ...RedefinableElement) RedefinableElement
```

Initial defines the initial state for a composite state or the entire state machine.
When a composite state is entered, its initial state is automatically entered.

Example:

	hsm.State("operational",
	    hsm.State("idle"),
	    hsm.State("running"),
	    hsm.Initial("idle")
	)

#### On

```go
func On[T interface{ *Event | Event }](events ...T) RedefinableElement
```

On defines the events that can cause a transition.
Multiple events can be specified for a single transition.

Example:

	hsm.Transition(
	    hsm.On("start", "resume"),
	    hsm.Source("idle"),
	    hsm.Target("running")
	)

#### Source

```go
func Source[T interface{ RedefinableElement | string }](nameOrPartialElement T) RedefinableElement
```

Source specifies the source state of a transition.
It can be used within a Transition definition.

Example:

	hsm.Transition(
	    hsm.Source("idle"),
	    hsm.Target("running")
	)

#### State

```go
func State(name string, partialElements ...RedefinableElement) RedefinableElement
```

State creates a new state element with the given name and optional child elements.
States can have entry/exit actions, activities, and transitions.

Example:

	hsm.State("active",
	    hsm.Entry(func(ctx context.Context, hsm *MyHSM, event Event) {
	        log.Println("Entering active state")
	    }),
	    hsm.Activity(func(ctx context.Context, hsm *MyHSM, event Event) {
	        // Long-running activity
	    }),
	    hsm.Exit(func(ctx context.Context, hsm *MyHSM, event Event) {
	        log.Println("Exiting active state")
	    })
	)

#### Target

```go
func Target[T interface{ RedefinableElement | string }](nameOrPartialElement T) RedefinableElement
```

Target specifies the target state of a transition.
It can be used within a Transition definition.

Example:

	hsm.Transition(
	    hsm.Source("idle"),
	    hsm.Target("running")
	)

#### Transition

```go
func Transition[T interface{ RedefinableElement | string }](nameOrPartialElement T, partialElements ...RedefinableElement) RedefinableElement
```

Transition creates a new transition between states.
Transitions can have triggers, guards, and effects.

Example:

	hsm.Transition(
	    hsm.Trigger("submit"),
	    hsm.Source("draft"),
	    hsm.Target("review"),
	    hsm.Guard(func(ctx context.Context, hsm *MyHSM, event Event) bool {
	        return hsm.IsValid()
	    }),
	    hsm.Effect(func(ctx context.Context, hsm *MyHSM, event Event) {
	        log.Println("Transitioning from draft to review")
	    })
	)

#### When

```go
func When[T Instance](expr func(ctx context.Context, hsm T, event Event) <-chan struct{}) RedefinableElement
```


## type Snapshot

```go
type Snapshot struct {
	ID            string
	QualifiedName string
	State         string
	QueueLen      int
	Events        []EventDetail
}
```

### Functions returning Snapshot

#### TakeSnapshot

```go
func TakeSnapshot(ctx context.Context, hsm Instance) Snapshot
```


## type active

```go
type active struct {
	ctx
	cancel  context.CancelFunc
	channel chan struct{}
}
```

## type after

```go
type after struct {
	entered    sync.Map
	exited     sync.Map
	dispatched sync.Map
	processed  sync.Map
	executed   sync.Map
}
```

## type behavior

```go
type behavior[T Instance] struct {
	element
	operation Operation[T]
}
```

### Methods

#### behavior.Id

```go
func () Id() string
```

#### behavior.Kind

```go
func () Kind() uint64
```

#### behavior.Name

```go
func () Name() string
```

#### behavior.Owner

```go
func () Owner() string
```

#### behavior.QualifiedName

```go
func () QualifiedName() string
```


## type constraint

```go
type constraint[T Instance] struct {
	element
	expression Expression[T]
}
```

### Methods

#### constraint.Id

```go
func () Id() string
```

#### constraint.Kind

```go
func () Kind() uint64
```

#### constraint.Name

```go
func () Name() string
```

#### constraint.Owner

```go
func () Owner() string
```

#### constraint.QualifiedName

```go
func () QualifiedName() string
```


## type ctx

```go
type ctx = context.Context
```

## type element

```go
type element struct {
	kind          uint64
	qualifiedName string
	id            string
}
```

### Methods

#### element.Id

```go
func () Id() string
```

#### element.Kind

```go
func () Kind() uint64
```

#### element.Name

```go
func () Name() string
```

#### element.Owner

```go
func () Owner() string
```

#### element.QualifiedName

```go
func () QualifiedName() string
```


## type hsm

```go
type hsm[T Instance] struct {
	behavior[T]
	state      atomic.Value
	context    context.Context
	cancel     context.CancelFunc
	model      *Model
	active     map[string]*active
	queue      queue
	instance   T
	timeouts   timeouts
	processing mutex
	after      after
}
```

### Methods

#### hsm.Context

```go
func () Context() context.Context
```

#### hsm.Id

```go
func () Id() string
```

#### hsm.Kind

```go
func () Kind() uint64
```

#### hsm.Name

```go
func () Name() string
```

#### hsm.Owner

```go
func () Owner() string
```

#### hsm.QualifiedName

```go
func () QualifiedName() string
```

#### hsm.State

```go
func () State() string
```

#### hsm.activate

```go
func () activate(ctx context.Context, element Element) *active
```

#### hsm.bind

```go
func () bind(instance Instance)
```

#### hsm.channels

```go
func () channels() *after
```

#### hsm.dispatch

```go
func () dispatch(ctx context.Context, event Event) <-chan struct{}
```

#### hsm.enter

```go
func () enter(ctx context.Context, element Element, event *Event, defaultEntry bool) Element
```

#### hsm.evaluate

```go
func () evaluate(ctx context.Context, guard *constraint[T], event *Event) bool
```

#### hsm.execute

```go
func () execute(ctx context.Context, element *behavior[T], event *Event)
```

#### hsm.executeAll

```go
func () executeAll(ctx context.Context, names []string, event *Event)
```

#### hsm.exit

```go
func () exit(ctx context.Context, element Element, event *Event)
```

#### hsm.process

```go
func () process(ctx context.Context)
```

#### hsm.restart

```go
func () restart(ctx context.Context, maybeData ...any) <-chan struct{}
```

#### hsm.start

```go
func () start(ctx context.Context, instance Instance, event *Event)
```

#### hsm.stop

```go
func () stop(ctx context.Context) <-chan struct{}
```

#### hsm.takeSnapshot

```go
func () takeSnapshot() Snapshot
```

#### hsm.terminate

```go
func () terminate(ctx context.Context, element Element)
```

#### hsm.transition

```go
func () transition(ctx context.Context, current Element, transition *transition, event *Event) Element
```

#### hsm.wait

```go
func () wait() <-chan struct{}
```


## type instance

```go
type instance = Instance
```

## type key

```go
type key[T any] struct{}
```

## type mutex

```go
type mutex struct {
	internal sync.RWMutex
	signal   atomic.Value
}
```

### Methods

#### mutex.tryLock

```go
func () tryLock() bool
```

#### mutex.wLock

```go
func () wLock()
```

#### mutex.wUnlock

```go
func () wUnlock()
```

#### mutex.wait

```go
func () wait() <-chan struct{}
```


## type paths

```go
type paths struct {
	enter []string
	exit  []string
}
```

## type queue

```go
type queue struct {
	mutex sync.RWMutex
	lifo  []Event // lifo
	fifo  []Event // fifo

}
```

### Methods

#### queue.len

```go
func () len() int
```

#### queue.pop

```go
func () pop() (Event, bool)
```

#### queue.push

```go
func () push(events ...Event)
```


## type state

```go
type state struct {
	vertex
	initial    string
	entry      []string
	exit       []string
	activities []string
	deferred   []string
}
```

### Methods

#### state.Activities

```go
func () Activities() []string
```

#### state.Entry

```go
func () Entry() []string
```

#### state.Exit

```go
func () Exit() []string
```

#### state.Id

```go
func () Id() string
```

#### state.Kind

```go
func () Kind() uint64
```

#### state.Name

```go
func () Name() string
```

#### state.Owner

```go
func () Owner() string
```

#### state.QualifiedName

```go
func () QualifiedName() string
```

#### state.Transitions

```go
func () Transitions() []string
```


## type timeouts

```go
type timeouts struct {
	activity time.Duration
}
```

## type transition

```go
type transition struct {
	element
	source string
	target string
	guard  string
	effect []string
	events []string
	paths  map[string]paths
}
```

### Methods

#### transition.Effect

```go
func () Effect() []string
```

#### transition.Events

```go
func () Events() []string
```

#### transition.Guard

```go
func () Guard() string
```

#### transition.Id

```go
func () Id() string
```

#### transition.Kind

```go
func () Kind() uint64
```

#### transition.Name

```go
func () Name() string
```

#### transition.Owner

```go
func () Owner() string
```

#### transition.QualifiedName

```go
func () QualifiedName() string
```

#### transition.Source

```go
func () Source() string
```

#### transition.Target

```go
func () Target() string
```


## type vertex

```go
type vertex struct {
	element
	transitions []string
}
```

### Methods

#### vertex.Id

```go
func () Id() string
```

#### vertex.Kind

```go
func () Kind() uint64
```

#### vertex.Name

```go
func () Name() string
```

#### vertex.Owner

```go
func () Owner() string
```

#### vertex.QualifiedName

```go
func () QualifiedName() string
```

#### vertex.Transitions

```go
func () Transitions() []string
```


