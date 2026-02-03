// Package hsm provides a powerful hierarchical state machine (HSM) implementation for Go.
//
// # Overview
//
// It enables modeling complex state-driven systems with features like hierarchical states,
// entry/exit actions, guard conditions, and event-driven transitions. The implementation
// follows the Unified DSK (Domain Specific Kit) specification, ensuring consistency across
// platforms.
//
// # Features
//
//   - **Hierarchical States**: Support for nested states and regions.
//   - **Event-Driven**: Asynchronous event processing with context propagation.
//   - **Guards & Actions**: Flexible functional definitions for transition guards and state actions.
//   - **Type Safe**: Generics-based implementation for state context.
//
// # Usage
//
// Define your state machine structure and behavior using the declarative builder pattern:
//
//	type MyHSM struct {
//	    hsm.HSM
//	    counter int
//	}
//
//	model := hsm.Define(
//	    "example",
//	    hsm.State("foo"),
//	    hsm.State("bar"),
//	    hsm.Transition(
//	        hsm.Trigger("moveToBar"),
//	        hsm.Source("foo"),
//	        hsm.Target("bar"),
//	    ),
//	    hsm.Initial("foo"),
//	)
//
//	// Start the state machine
//	sm := hsm.Start(context.Background(), &MyHSM{}, &model)
//	sm.Dispatch(hsm.Event{Name: "moveToBar"})
package hsm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stateforward/hsm.go/kind"
	"github.com/stateforward/hsm.go/muid"
)

// Kind constants define the HSM type hierarchy using bit-packed inheritance.
// Each Kind encodes its own ID and the IDs of its ancestor types, enabling
// efficient type checking via kind.Is(). The hierarchy follows UML state machine
// concepts where elements can inherit from multiple parent kinds.
var (
	// NullKind represents the absence of a kind or an uninitialized kind value.
	NullKind = kind.Make()
	// ElementKind is the base kind for all HSM elements. Every structural
	// component in the state machine hierarchy derives from ElementKind.
	ElementKind = kind.Make()
	// NamespaceKind represents elements that can contain named children,
	// enabling hierarchical name resolution for states and state machines.
	NamespaceKind = kind.Make(ElementKind)
	// VertexKind is the base kind for nodes in the state graph that can be
	// sources or targets of transitions, including states and pseudostates.
	VertexKind = kind.Make(ElementKind)
	// ConstraintKind represents guard conditions that control transition firing.
	ConstraintKind = kind.Make(ElementKind)
	// BehaviorKind represents executable behaviors such as entry, exit, and
	// transition effects that run during state machine execution.
	BehaviorKind = kind.Make(ElementKind)
	// ConcurrentKind represents behaviors that support concurrent execution
	// of multiple orthogonal regions.
	ConcurrentKind = kind.Make(BehaviorKind)
	// StateMachineKind represents the top-level state machine container that
	// owns regions, states, and transitions. Inherits from both ConcurrentKind
	// (for orthogonal regions) and NamespaceKind (for named child lookup).
	StateMachineKind = kind.Make(ConcurrentKind, NamespaceKind)
	// StateKind represents a state vertex that can contain nested regions,
	// entry/exit behaviors, and internal transitions. Inherits from VertexKind
	// (as a graph node) and NamespaceKind (as a container for child elements).
	StateKind = kind.Make(VertexKind, NamespaceKind)
	// RegionKind represents an orthogonal region within a composite state or
	// state machine, containing vertices and transitions.
	RegionKind = kind.Make(ElementKind)
	// TransitionKind is the base kind for all transitions between vertices,
	// representing edges in the state graph.
	TransitionKind = kind.Make(ElementKind)
	// InternalKind represents internal transitions that execute without
	// exiting or re-entering the containing state.
	InternalKind = kind.Make(TransitionKind)
	// ExternalKind represents external transitions that exit the source state
	// and enter the target state, triggering exit and entry behaviors.
	ExternalKind = kind.Make(TransitionKind)
	// LocalKind represents local transitions that do not exit the containing
	// composite state when transitioning between its substates.
	LocalKind = kind.Make(TransitionKind)
	// SelfKind represents self-transitions where the source and target are
	// the same state, triggering exit and re-entry of that state.
	SelfKind = kind.Make(TransitionKind)
	// EventKind is the base kind for all events that can trigger transitions
	// in the state machine.
	EventKind = kind.Make(ElementKind)
	// TimeEventKind represents events triggered after a specified duration,
	// used for timeout-based transitions.
	TimeEventKind = kind.Make(EventKind)
	// CompletionEventKind represents events automatically generated when a
	// state completes all its internal activities or reaches a final state.
	CompletionEventKind = kind.Make(EventKind)
	// ChangeEventKind represents events triggered when a boolean condition
	// becomes true, enabling data-driven transitions.
	ChangeEventKind = kind.Make(EventKind)
	// CallEventKind represents events triggered by method calls on the state
	// machine, enabling synchronous event dispatch.
	CallEventKind = kind.Make(EventKind)
	// ErrorEventKind represents events generated when an error occurs during
	// state machine execution. Inherits from CompletionEventKind as errors
	// typically complete the current processing.
	ErrorEventKind = kind.Make(CompletionEventKind)
	// PseudostateKind is the base kind for transient vertices that perform
	// control flow logic without representing stable states.
	PseudostateKind = kind.Make(VertexKind)
	// InitialKind represents the initial pseudostate that indicates the
	// default starting state when entering a region.
	InitialKind = kind.Make(PseudostateKind)
	// FinalStateKind represents a final state indicating that the enclosing
	// region has completed its execution.
	FinalStateKind = kind.Make(StateKind)
	// ChoiceKind represents a choice pseudostate that evaluates guards to
	// select among multiple outgoing transitions dynamically.
	ChoiceKind = kind.Make(PseudostateKind)
	// ShallowHistoryKind represents a shallow history pseudostate that
	// remembers the most recently active direct substate of its region.
	ShallowHistoryKind = kind.Make(PseudostateKind)
	// DeepHistoryKind represents a deep history pseudostate that remembers
	// the full active state configuration within its region recursively.
	DeepHistoryKind = kind.Make(PseudostateKind)
	// CustomKind represents user-defined element types for extending the
	// HSM framework with application-specific constructs.
	CustomKind = kind.Make(ElementKind)
)

// Error variables for common HSM error conditions.
// These sentinel errors can be checked using errors.Is for specific error handling.
var (
	// ErrNilHSM is returned when an operation is attempted on a nil state machine.
	ErrNilHSM = errors.New("hsm is nil")
	// ErrInvalidState is returned when attempting to access or transition to an invalid state.
	ErrInvalidState = errors.New("invalid state")
	// ErrMissingHSM is returned when the HSM instance cannot be found in the context.
	ErrMissingHSM = errors.New("missing hsm in context")
	// ErrMissingOperation is returned when an operation callback is required but not provided.
	ErrMissingOperation = errors.New("missing operation")
	// ErrInvalidOperation is returned when an operation callback has an unsupported function signature.
	ErrInvalidOperation = errors.New("invalid operation")
	// ErrAlreadyStarted is returned when a state machine has already been started.
	ErrAlreadyStarted = errors.New("hsm already started")
)

/******* Element *******/

type element struct {
	kind          uint64
	qualifiedName string
	id            string
}

func (element *element) Kind() uint64 {
	if element == nil {
		return 0
	}
	return element.kind
}

func (element *element) Owner() string {
	if element == nil || element.qualifiedName == "/" {
		return ""
	}
	return path.Dir(element.qualifiedName)
}

func (element *element) Id() string {
	if element == nil {
		return ""
	}
	return element.id
}

func (element *element) Name() string {
	if element == nil {
		return ""
	}
	return path.Base(element.qualifiedName)
}

func (element *element) QualifiedName() string {
	if element == nil {
		return ""
	}
	return element.qualifiedName
}

type Element interface {
	Id() string
	Kind() uint64
	Owner() string
	QualifiedName() string
	Name() string
}

/******* Model *******/

// Element represents a named element in the state machine hierarchy.
// It provides basic identification and naming capabilities.

// Model represents the complete state machine model definition.
// It contains the root state and maintains a namespace of all elements.
type Model struct {
	state
	members    map[string]Element
	events     map[string]*Event
	attributes map[string]*attribute
	operations map[string]*operationDef
	elements   []RedefinableElement
	// TransitionMap provides fast lookup of transitions by state and event name
	TransitionMap map[string]map[string][]*transition // stateQualifiedName -> eventName -> transitions
	// DeferredMap provides fast lookup of deferred events by state
	DeferredMap map[string]map[string]struct{} // stateQualifiedName -> Set<deferredEventNames>
}

func (model *Model) Members() map[string]Element {
	return model.members
}

func (model *Model) push(partial RedefinableElement) {
	model.elements = append(model.elements, partial)
}

// RedefinableElement is a function type that modifies a Model by adding or updating elements.
// It's used to build the state machine structure in a declarative way.
type RedefinableElement = func(model *Model, stack []Element) Element

/******* Vertex *******/

type vertex struct {
	element
	transitions []string
}

func (vertex *vertex) Transitions() []string {
	return vertex.transitions
}

/******* State *******/

type state struct {
	vertex
	initial    string
	entry      []string
	exit       []string
	activities []string
	deferred   []string
}

func (state *state) Entry() []string {
	return state.entry
}

func (state *state) Activities() []string {
	return state.activities
}

func (state *state) Exit() []string {
	return state.exit
}

/******* Transition *******/

type paths struct {
	enter []string
	exit  []string
}

type transition struct {
	element
	source string
	target string
	guard  string
	effect []string
	events []string
	paths  map[string]paths
}

func (transition *transition) Guard() string {
	return transition.guard
}

func (transition *transition) Effect() []string {
	return transition.effect
}

func (transition *transition) Events() []string {
	return transition.events
}

func (transition *transition) Source() string {
	return transition.source
}

func (transition *transition) Target() string {
	return transition.target
}

/******* Behavior *******/

// OperationFunc is a function type that performs an action on a state machine.
// Operations are used for state entry/exit behaviors, transition effects,
// and activity functions. They receive the current context, state machine
// instance, and the triggering event.
type OperationFunc[T Instance] func(ctx context.Context, hsm T, event Event)

// ExpressionFunc is a function type that evaluates a condition on a state machine.
// Expressions are used for transition guards to determine whether a transition
// should be taken. They receive the current context, state machine instance,
// and the triggering event, returning true if the condition is satisfied.
type ExpressionFunc[T Instance] func(ctx context.Context, hsm T, event Event) bool

type behavior[T Instance] struct {
	element
	operation OperationFunc[T]
}

/******* Constraint *******/

type constraint[T Instance] struct {
	element
	expression ExpressionFunc[T]
}

/******* Attribute & OperationFunc *******/

type attribute struct {
	name         string
	defaultValue any
	hasDefault   bool
}

type operationDef struct {
	name    string
	fn      any
	fnValue reflect.Value
	fnType  reflect.Type
}

/******* Events *******/

// Event represents a trigger that can cause state transitions in the state machine.
// Events can carry data and have completion tracking through the Done channel.

type Event struct {
	Kind   uint64 `xml:"kind,attr" json:"kind"`
	Name   string `xml:"name,attr" json:"name"`
	ID     string `xml:"id,attr" json:"id"`
	Source string `xml:"source,attr,omitempty" json:"source,omitempty"`
	Target string `xml:"target,attr,omitempty" json:"target,omitempty"`
	Data   any    `xml:"data" json:"data"`
	Schema any    `xml:"schema" json:"schema"`
}

func (e Event) WithData(data any) Event {
	return Event{
		Kind:   e.Kind,
		Name:   e.Name,
		ID:     e.ID,
		Source: e.Source,
		Target: e.Target,
		Data:   data,
		Schema: e.Schema,
	}
}

func (e Event) WithDataAndID(data any, id string) Event {
	return Event{
		Kind:   e.Kind,
		Name:   e.Name,
		ID:     id,
		Data:   data,
		Schema: e.Schema,
	}
}

// Built-in event types and special duration constants used by the HSM runtime.
var (
	// InitialEvent is the completion event dispatched when a state machine starts.
	// It triggers the initial transition from the initial pseudostate to the first active state.
	InitialEvent = Event{
		Name: "hsm_initial",
		Kind: CompletionEventKind,
	}
	// ErrorEvent is dispatched when an error occurs during state machine execution,
	// such as panics in concurrent behaviors or timeout errors. Use On(ErrorEvent) to handle errors.
	ErrorEvent = Event{
		Name: "hsm_error",
		Kind: ErrorEventKind,
	}
	// AnyEvent is a wildcard event that matches any event not explicitly handled.
	// Transitions using On(AnyEvent) are only taken when no specific event transition matches.
	AnyEvent = Event{
		Name: "*",
		Kind: EventKind,
	}
	// FinalEvent is the completion event dispatched when a state reaches its final state.
	// It triggers exit behaviors and propagates completion to parent regions.
	FinalEvent = Event{
		Name: "hsm_final",
		Kind: CompletionEventKind,
	}
	// InfiniteDuration represents an unbounded duration for timeout operations.
	// Use this when a behavior should run indefinitely without timing out.
	InfiniteDuration = time.Duration(-1)
)

func qualifyModelName(modelQualified, name string) string {
	if name == "" {
		return ""
	}
	if path.IsAbs(name) {
		return path.Clean(name)
	}
	return path.Join(modelQualified, name)
}

func registerEvent(traceback func(error), model *Model, event *Event) {
	if event == nil {
		return
	}
	if model.events == nil {
		model.events = map[string]*Event{}
	}
	if existing := model.events[event.Name]; existing != nil {
		existingKind := existing.Kind
		if existingKind == 0 {
			existingKind = EventKind
		}
		newKind := event.Kind
		if newKind == 0 {
			newKind = EventKind
		}
		if existingKind != newKind {
			traceback(fmt.Errorf("event \"%s\" already defined with a different kind", event.Name))
		}
		return
	}
	model.events[event.Name] = event
}

// AttributeChange is the payload for attribute change events.
type AttributeChange struct {
	Name string
	Old  any
	New  any
}

// CallData is the payload for call events.
type CallData struct {
	Name string
	Args []any
}

var closedChannel = func() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()

type queue struct {
	mutex sync.RWMutex
	lifo  []Event // lifo
	fifo  []Event // fifo

}

var empty = Event{}

func (q *queue) len() int {
	q.mutex.RLock()
	defer q.mutex.RUnlock()
	return len(q.fifo) + len(q.lifo)
}

func (q *queue) pop() (Event, bool) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	switch {
	case len(q.lifo) > 0:
		event := q.lifo[len(q.lifo)-1]
		q.lifo = q.lifo[:len(q.lifo)-1]
		return event, true
	case len(q.fifo) > 0:
		event := q.fifo[0]
		q.fifo = q.fifo[1:]
		return event, true
	default:
		return empty, false
	}
}

func (q *queue) push(events ...Event) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	for _, event := range events {
		if kind.Is(event.Kind, CompletionEventKind) {
			q.lifo = append(q.lifo, event)
		} else {
			q.fifo = append(q.fifo, event)
		}
	}
}

func apply(model *Model, stack []Element, partials ...RedefinableElement) {
	for _, partial := range partials {
		partial(model, stack)
	}
}

// Define creates a new state machine model with the given name and elements.
// The first argument can be either a string name or a RedefinableElement.
// Additional elements are added to the model in the order they are specified.
//
// Example:
//
//	model := hsm.Define(
//	    "traffic_light",
//	    hsm.State("red"),
//	    hsm.State("yellow"),
//	    hsm.State("green"),
//	    hsm.Initial("red")
//	)
func Define(name string, redefinableElements ...RedefinableElement) Model {
	model := Model{
		state: state{
			vertex: vertex{element: element{kind: StateKind, qualifiedName: path.Join("/", name), id: name}, transitions: []string{}},
		},
		elements:      redefinableElements,
		events:        map[string]*Event{},
		attributes:    map[string]*attribute{},
		operations:    map[string]*operationDef{},
		TransitionMap: map[string]map[string][]*transition{},
		DeferredMap:   map[string]map[string]struct{}{},
	}
	model.members = map[string]Element{
		model.qualifiedName: &model.state,
	}
	stack := []Element{&model.state}
	for len(model.elements) > 0 {
		elements := model.elements
		model.elements = []RedefinableElement{}
		apply(&model, stack, elements...)
	}

	if model.state.initial == "" {
		panic(fmt.Errorf("initial state is required for state machine %s", model.state.id))
	}
	if len(model.state.entry) > 0 {
		panic(fmt.Errorf("entry actions are not allowed on top level state machine %s", model.state.id))
	}
	if len(model.state.exit) > 0 {
		panic(fmt.Errorf("exit actions are not allowed on top level state machine %s", model.state.id))
	}
	buildCaches(&model)
	return model
}

func buildCaches(model *Model) {
	type Vertex interface {
		Transitions() []string
	}
	// Build transitionMap and deferredMap for fast lookups
	// For each state, we build a map that includes ALL transitions accessible from that state
	// by walking up the hierarchy. This gives us O(1) lookup without hierarchy traversal at runtime.

	for qualifiedName, element := range model.members {
		// Build transition maps for all vertex types (states, pseudostates, etc)
		if _, ok := element.(Vertex); !ok {
			continue
		}

		// Initialize maps for this state
		model.TransitionMap[qualifiedName] = make(map[string][]*transition)
		model.DeferredMap[qualifiedName] = make(map[string]struct{})

		// Build a path from root to current state
		var pathToState []string
		currentPath := qualifiedName
		for currentPath != "" {
			pathToState = append([]string{currentPath}, pathToState...)
			if currentPath == model.qualifiedName {
				break
			}
			currentPath = path.Dir(currentPath)
		}

		// Walk from state to root, so more specific transitions come first (higher priority)
		for i := len(pathToState) - 1; i >= 0; i-- {
			statePath := pathToState[i]
			currentElement := model.members[statePath]
			if currentElement == nil {
				continue
			}

			// Collect transitions at this level
			if vertex, ok := currentElement.(Vertex); ok {
				for _, transitionQualifiedName := range vertex.Transitions() {
					if trans := get[*transition](model, transitionQualifiedName); trans != nil {
						// Add all transitions - more specific ones will be checked first in process
						for _, eventName := range trans.events {
							model.TransitionMap[qualifiedName][eventName] = append(model.TransitionMap[qualifiedName][eventName], trans)
						}
					}
				}
			}

			// Collect deferred events at this level
			if state, ok := currentElement.(*state); ok {
				for _, deferredEvent := range state.deferred {
					// Check if this event has a transition at the current state level
					// If so, it's not deferred from this state
					hasTransition := false
					if vertex, ok := element.(Vertex); ok {
						for _, transQualifiedName := range vertex.Transitions() {
							if trans := get[*transition](model, transQualifiedName); trans != nil {
								if slices.Contains(trans.events, deferredEvent) {
									hasTransition = true
								}
							}
							if hasTransition {
								break
							}
						}
					}

					if !hasTransition {
						model.DeferredMap[qualifiedName][deferredEvent] = struct{}{}
					}
				}
			}
		}
	}
}

func find(stack []Element, maybeKinds ...uint64) Element {
	for i := len(stack) - 1; i >= 0; i-- {
		if kind.Is(stack[i].Kind(), maybeKinds...) {
			return stack[i]
		}
	}
	return nil
}

func traceback(maybeError ...error) func(err error) {
	_, file, line, _ := runtime.Caller(2)
	fn := func(err error) {
		panic(fmt.Sprintf("%s:%d: %v", file, line, err))
	}
	if len(maybeError) > 0 {
		fn(maybeError[0])
	}
	return fn
}

func get[T Element](model *Model, name string) T {
	var zero T
	if name == "" {
		return zero
	}
	if element, ok := model.members[name]; ok {
		typed, ok := element.(T)
		if ok {
			return typed
		}
	}
	return zero
}

func getFunctionName(fn any) string {
	if fn == nil {
		return ""
	}
	return path.Base(runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name())
}

// State creates a new state element with the given name and optional child elements.
// States can have entry/exit actions, activities, and transitions.
//
// Example:
//
//	hsm.State("active",
//	    hsm.Entry(func(ctx context.Context, hsm *MyHSM, event Event) {
//	        log.Println("Entering active state")
//	    }),
//	    hsm.Activity(func(ctx context.Context, hsm *MyHSM, event Event) {
//	        // Long-running activity
//	    }),
//	    hsm.Exit(func(ctx context.Context, hsm *MyHSM, event Event) {
//	        log.Println("Exiting active state")
//	    })
//	)
func State(name string, partialElements ...RedefinableElement) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, NamespaceKind)
		if owner == nil {
			traceback(fmt.Errorf("state \"%s\" must be called within Define() or State()", name))
		}
		element := &state{
			vertex: vertex{element: element{kind: StateKind, qualifiedName: path.Join(owner.QualifiedName(), name)}, transitions: []string{}},
		}
		if _, ok := model.members[element.QualifiedName()]; ok {
			traceback(fmt.Errorf("state \"%s\" already defined", element.QualifiedName()))
		}
		model.members[element.QualifiedName()] = element
		stack = append(stack, element)
		apply(model, stack, partialElements...)
		model.push(func(model *Model, stack []Element) Element {
			// Sort transitions so wildcard events are at the end
			slices.SortStableFunc(element.transitions, func(i, j string) int {
				transitionI := get[*transition](model, i)
				if transitionI == nil {
					traceback(fmt.Errorf("missing transition \"%s\" for state \"%s\"", i, element.QualifiedName()))
					return 1 // because the linter doesn't know that traceback will panic
				}
				transitionJ := get[*transition](model, j)
				if transitionJ == nil {
					traceback(fmt.Errorf("missing transition \"%s\" for state \"%s\"", j, element.QualifiedName()))
					return 1 // because the linter doesn't know that traceback will panic
				}
				// No wildcard prioritization needed anymore
				return 0
			})
			return element
		})
		return element
	}
}

// LCA finds the Lowest Common Ancestor between two qualified state names in a hierarchical state machine.
// It takes two qualified names 'a' and 'b' as strings and returns their closest common ancestor.
//
// For example:
// - LCA("/s/s1", "/s/s2") returns "/s"
// - LCA("/s/s1", "/s/s1/s11") returns "/s/s1"
// - LCA("/s/s1", "/s/s1") returns "/s/s1"
func LCA(a, b string) string {
	// if both are the same the lca is the parent
	if a == b {
		return path.Dir(a)
	}
	// if one is empty the lca is the other
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	// if the parents are the same the lca is the parent
	if path.Dir(a) == path.Dir(b) {
		return path.Dir(a)
	}
	// if a is an ancestor of b the lca is a
	if IsAncestor(a, b) {
		return a
	}
	// if b is an ancestor of a the lca is b
	if IsAncestor(b, a) {
		return b
	}
	// otherwise the lca is the lca of the parents
	return LCA(path.Dir(a), path.Dir(b))
}

// IsAncestor checks whether current is an ancestor of target in the state hierarchy.
// It returns true if current appears in the path from the root to target's parent.
// Returns false if current equals target, or if either path is "." (relative root).
// The root path "/" is considered an ancestor of all other paths.
func IsAncestor(current, target string) bool {
	current = path.Clean(current)
	target = path.Clean(target)
	if current == target || current == "." || target == "." {
		return false
	}
	if current == "/" {
		return true
	}
	parent := path.Dir(target)
	for parent != "/" {
		if parent == current {
			return true
		}
		parent = path.Dir(parent)
	}
	return false
}

// Transition creates a new transition between states.
// Transitions can have triggers, guards, and effects.
//
// Example:
//
//	hsm.Transition(
//	    hsm.Trigger("submit"),
//	    hsm.Source("draft"),
//	    hsm.Target("review"),
//	    hsm.Guard(func(ctx context.Context, hsm *MyHSM, event Event) bool {
//	        return hsm.IsValid()
//	    }),
//	    hsm.Effect(func(ctx context.Context, hsm *MyHSM, event Event) {
//	        log.Println("Transitioning from draft to review")
//	    })
//	)
func Transition[T interface{ RedefinableElement | string }](nameOrPartialElement T, partialElements ...RedefinableElement) RedefinableElement {
	name := ""
	switch any(nameOrPartialElement).(type) {
	case string:
		name = any(nameOrPartialElement).(string)
	case RedefinableElement:
		partialElements = append([]RedefinableElement{any(nameOrPartialElement).(RedefinableElement)}, partialElements...)
	}
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, VertexKind)
		if name == "" {
			name = fmt.Sprintf("transition_%d", len(model.members))
		}
		if owner == nil {
			traceback(fmt.Errorf("transition \"%s\" must be called within a State() or Define()", name))
		}
		transition := &transition{
			events: []string{},
			element: element{
				kind:          TransitionKind,
				qualifiedName: path.Join(owner.QualifiedName(), name),
			},
			source: ".",
			paths:  map[string]paths{},
		}
		model.members[transition.QualifiedName()] = transition
		stack = append(stack, transition)
		apply(model, stack, partialElements...)
		if transition.source == "." || transition.source == "" {
			transition.source = owner.QualifiedName()
		}
		sourceElement, ok := model.members[transition.source]
		if !ok {
			traceback(fmt.Errorf("missing source \"%s\" for transition \"%s\"", transition.source, transition.QualifiedName()))
		}
		switch source := sourceElement.(type) {
		case *state:
			source.transitions = append(source.transitions, transition.QualifiedName())
		case *vertex:
			source.transitions = append(source.transitions, transition.QualifiedName())
		}
		if len(transition.events) == 0 && !kind.Is(sourceElement.Kind(), PseudostateKind) {
			// TODO: completion transition
			// qualifiedName := path.Join(transition.source, ".completion")
			// transition.events = append(transition.events, &event{
			// 	element: element{kind: kind.CompletionEvent, qualifiedName: qualifiedName},
			// })
			traceback(fmt.Errorf("completion transition not implemented"))
		}
		if transition.target == transition.source {
			transition.kind = SelfKind
		} else if transition.target == "" {
			transition.kind = InternalKind
		} else if IsAncestor(transition.source, transition.target) {
			transition.kind = LocalKind
		} else {
			transition.kind = ExternalKind
		}
		enter := []string{}
		entering := transition.target
		lca := LCA(transition.source, transition.target)
		for entering != lca && entering != model.qualifiedName && entering != "" {
			enter = append([]string{entering}, enter...)
			entering = path.Dir(entering)
		}
		if kind.Is(transition.kind, SelfKind) {
			enter = append(enter, sourceElement.QualifiedName())
		}
		if kind.Is(sourceElement.Kind(), InitialKind) {
			transition.paths[path.Dir(sourceElement.QualifiedName())] = paths{
				enter: enter,
				exit:  []string{sourceElement.QualifiedName()},
			}
		} else {
			model.push(func(model *Model, stack []Element) Element {
				if transition.source == model.QualifiedName() && transition.target != "" {
					traceback(fmt.Errorf("top level transitions must have a source and target, or no source and target"))
				}
				if kind.Is(transition.kind, InternalKind) && len(transition.effect) == 0 {
					traceback(fmt.Errorf("internal transitions require an effect"))
				}
				// precompute transition paths for the source state and nested states
				for qualifiedName, element := range model.members {
					if strings.HasPrefix(qualifiedName, transition.source) && kind.Is(element.Kind(), VertexKind) {
						exit := []string{}
						if transition.kind != InternalKind {
							exiting := element.QualifiedName()
							for exiting != lca && exiting != "" {
								exit = append(exit, exiting)
								if exiting == model.qualifiedName {
									break
								}
								exiting = path.Dir(exiting)
							}
						}
						transition.paths[element.QualifiedName()] = paths{
							enter: enter,
							exit:  exit,
						}
					}

				}
				return transition
			})
		}

		return transition
	}
}

// Source specifies the source state of a transition.
// It can be used within a Transition definition.
//
// Example:
//
//	hsm.Transition(
//	    hsm.Source("idle"),
//	    hsm.Target("running")
//	)
func Source[T interface{ RedefinableElement | string }](nameOrPartialElement T) RedefinableElement {
	// Capture the stack depth for use in traceback
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, TransitionKind)
		if owner == nil {
			traceback(fmt.Errorf("hsm.Source() must be called within a hsm.Transition()"))
		}
		transition := owner.(*transition)
		if transition.source != "." && transition.source != "" {
			traceback(fmt.Errorf("transition \"%s\" already has a source \"%s\"", transition.QualifiedName(), transition.source))
		}
		var name string
		switch any(nameOrPartialElement).(type) {
		case string:
			name = any(nameOrPartialElement).(string)
			if !path.IsAbs(name) {
				if ancestor := find(stack, StateKind); ancestor != nil {
					name = path.Join(ancestor.QualifiedName(), name)
				}
			} else if !IsAncestor(model.qualifiedName, name) {
				name = path.Join(model.qualifiedName, name)
			}
			// push a validation step to ensure the source exists after the model is built
			model.push(func(model *Model, stack []Element) Element {
				if _, ok := model.members[name]; !ok {
					traceback(fmt.Errorf("missing source \"%s\" for transition \"%s\"", name, transition.QualifiedName()))
				}
				return owner
			})
		case RedefinableElement:
			element := any(nameOrPartialElement).(RedefinableElement)(model, stack)
			if element == nil {
				traceback(fmt.Errorf("transition \"%s\" source is nil", transition.QualifiedName()))
			}
			name = element.QualifiedName()
		}
		transition.source = name
		return owner
	}
}

// Defer schedules events to be processed after the current state is exited.
//
// Example:
//
//	hsm.Defer(hsm.Event{Name: "event_name"})
func Defer[T interface {
	string | *Event | Event
}](events ...T) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		state, ok := find(stack, StateKind).(*state)
		if !ok {
			traceback(fmt.Errorf("defer must be called within a State"))
		}
		for _, event := range events {
			switch evt := any(event).(type) {
			case string:
				state.deferred = append(state.deferred, evt)
			case *Event:
				state.deferred = append(state.deferred, evt.Name)
			case Event:
				state.deferred = append(state.deferred, evt.Name)
			default:
				traceback(fmt.Errorf("defer must be called with a string, *Event, or Event"))
			}
		}
		return state
	}
}

// Target specifies the target state of a transition.
// It can be used within a Transition definition.
//
// Example:
//
//	hsm.Transition(
//	    hsm.Source("idle"),
//	    hsm.Target("running")
//	)
func Target[T interface{ RedefinableElement | string }](nameOrPartialElement T) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, TransitionKind)
		if owner == nil {
			traceback(fmt.Errorf("Target() must be called within Transition()"))
		}
		transition := owner.(*transition)
		if transition.target != "" {
			traceback(fmt.Errorf("transition \"%s\" already has target \"%s\"", transition.QualifiedName(), transition.target))
		}
		var qualifiedName string
		switch target := any(nameOrPartialElement).(type) {
		case string:
			qualifiedName = target
			if !path.IsAbs(qualifiedName) {
				if ancestor := find(stack, StateKind); ancestor != nil {
					qualifiedName = path.Join(ancestor.QualifiedName(), qualifiedName)
				}
			} else if !IsAncestor(model.qualifiedName, qualifiedName) {
				qualifiedName = path.Join(model.qualifiedName, qualifiedName)
			}
			// push a validation step to ensure the target exists after the model is built
			model.push(func(model *Model, stack []Element) Element {
				if _, exists := model.members[qualifiedName]; !exists {
					traceback(fmt.Errorf("missing target \"%s\" for transition \"%s\"", target, transition.QualifiedName()))
				}
				return transition
			})
		case RedefinableElement:
			targetElement := target(model, stack)
			if targetElement == nil {
				traceback(fmt.Errorf("transition \"%s\" target is nil", transition.QualifiedName()))
			}
			qualifiedName = targetElement.QualifiedName()
		}

		transition.target = qualifiedName
		return transition
	}
}

// Effect defines an action to be executed during a transition.
// The effect function is called after exiting the source state and before entering the target state.
//
// Example:
//
//	hsm.Effect(func(ctx context.Context, hsm *MyHSM, event Event) {
//	    log.Printf("Transitioning with event: %s", event.Name)
//	})
func Effect[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner, ok := find(stack, TransitionKind).(*transition)
		if !ok {
			traceback(fmt.Errorf("effect must be called within a Transition"))
		}
		for _, fn := range funcs {
			name := getFunctionName(fn)
			behavior := &behavior[T]{
				element:   element{kind: BehaviorKind, qualifiedName: path.Join(owner.QualifiedName(), name)},
				operation: fn,
			}
			model.members[behavior.QualifiedName()] = behavior
			owner.effect = append(owner.effect, behavior.QualifiedName())
		}
		return owner
	}
}

// Guard defines a condition that must be true for a transition to be taken.
// If multiple transitions are possible, the first one with a satisfied guard is chosen.
//
// Example:
//
//	hsm.Guard(func(ctx context.Context, hsm *MyHSM, event Event) bool {
//	    return hsm.counter > 10
//	})
func Guard[T Instance](fn func(ctx context.Context, hsm T, event Event) bool) RedefinableElement {
	name := getFunctionName(fn)
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, TransitionKind)
		if owner == nil {
			traceback(fmt.Errorf("guard must be called within a Transition"))
		}
		constraint := &constraint[T]{
			element:    element{kind: ConstraintKind, qualifiedName: path.Join(owner.QualifiedName(), name)},
			expression: fn,
		}
		model.members[constraint.QualifiedName()] = constraint
		owner.(*transition).guard = constraint.QualifiedName()
		return owner
	}
}

// Attribute declares a model-level attribute with an optional default value.
// Attributes can be observed via OnSet("name") transitions and updated at runtime
// via Set / Instance.Set.
func Attribute(name string, maybeDefault ...any) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		if name == "" {
			traceback(fmt.Errorf("attribute name cannot be empty"))
		}
		qualifiedName := qualifyModelName(model.qualifiedName, name)
		if model.attributes == nil {
			model.attributes = map[string]*attribute{}
		}
		if _, exists := model.attributes[qualifiedName]; exists {
			traceback(fmt.Errorf("attribute \"%s\" already defined", qualifiedName))
		}
		attr := &attribute{name: qualifiedName}
		if len(maybeDefault) > 0 {
			attr.defaultValue = maybeDefault[0]
			attr.hasDefault = true
		}
		model.attributes[qualifiedName] = attr
		return nil
	}
}

// Operation declares a named callable for Call()/OnCall().
// Supported callables include function values and method expressions.
func Operation(name string, fn any) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		if name == "" {
			traceback(fmt.Errorf("operation name cannot be empty"))
		}
		qualifiedName := qualifyModelName(model.qualifiedName, name)
		if model.operations == nil {
			model.operations = map[string]*operationDef{}
		}
		if _, exists := model.operations[qualifiedName]; exists {
			traceback(fmt.Errorf("operation \"%s\" already defined", qualifiedName))
		}
		fnValue := reflect.ValueOf(fn)
		fnType := fnValue.Type()
		if !fnValue.IsValid() || fnType.Kind() != reflect.Func {
			traceback(fmt.Errorf("operation \"%s\" must be a function", qualifiedName))
		}
		model.operations[qualifiedName] = &operationDef{
			name:    qualifiedName,
			fn:      fn,
			fnValue: fnValue,
			fnType:  fnType,
		}
		return nil
	}
}

// Initial defines the initial state for a composite state or the entire state machine.
// When a composite state is entered, its initial state is automatically entered.
//
// Example:
//
//	hsm.State("operational",
//	    hsm.State("idle"),
//	    hsm.State("running"),
//	    hsm.Initial("idle")
//	)
func Initial(partialElements ...RedefinableElement) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, StateKind)
		if owner == nil {
			traceback(fmt.Errorf("initial must be called within a State or Model"))
		}
		initial := &vertex{
			element: element{kind: InitialKind, qualifiedName: path.Join(owner.QualifiedName(), ".initial")},
		}
		owner.(*state).initial = initial.QualifiedName()
		if model.members[initial.QualifiedName()] != nil {
			traceback(fmt.Errorf("initial \"%s\" state already exists for \"%s\"", initial.QualifiedName(), owner.QualifiedName()))
		}
		model.members[initial.QualifiedName()] = initial
		stack = append(stack, initial)
		transition := (Transition(Source(initial.QualifiedName()), append(partialElements, On(InitialEvent))...)(model, stack)).(*transition)
		// validation logic
		if transition.guard != "" {
			traceback(fmt.Errorf("initial \"%s\" cannot have a guard", initial.QualifiedName()))
		}
		if transition.events[0] != InitialEvent.Name {
			traceback(fmt.Errorf("initial \"%s\" must not have a trigger \"%s\"", initial.QualifiedName(), InitialEvent.Name))
		}
		if !strings.HasPrefix(transition.target, owner.QualifiedName()) {
			traceback(fmt.Errorf("initial \"%s\" must target a nested state not \"%s\"", initial.QualifiedName(), transition.target))
		}
		if len(initial.transitions) > 1 {
			traceback(fmt.Errorf("initial \"%s\" cannot have multiple transitions %v", initial.QualifiedName(), initial.transitions))
		}
		return transition
	}
}

// Choice creates a pseudo-state that enables dynamic branching based on guard conditions.
// The first transition with a satisfied guard condition is taken.
//
// Example:
//
//	hsm.Choice(
//	    hsm.Transition(
//	        hsm.Target("approved"),
//	        hsm.Guard(func(ctx context.Context, hsm *MyHSM, event Event) bool {
//	            return hsm.score > 700
//	        })
//	    ),
//	    hsm.Transition(
//	        hsm.Target("rejected")
//	    )
//	)
func Choice[T interface{ RedefinableElement | string }](elementOrName T, partialElements ...RedefinableElement) RedefinableElement {
	name := ""
	switch any(elementOrName).(type) {
	case string:
		name = any(elementOrName).(string)
	case RedefinableElement:
		partialElements = append([]RedefinableElement{any(elementOrName).(RedefinableElement)}, partialElements...)
	}
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, StateKind, TransitionKind)
		if owner == nil {
			traceback(fmt.Errorf("you must call Choice() within a State or Transition"))
		} else if kind.Is(owner.Kind(), TransitionKind) {
			transition := owner.(*transition)
			source := transition.source

			owner = model.members[source]
			if owner == nil {
				traceback(fmt.Errorf("transition \"%s\" targetting \"%s\" requires a source state when using Choice()", transition.QualifiedName(), transition.target))
			} else if kind.Is(owner.Kind(), PseudostateKind) {
				// pseudostates aren't a namespace, so we need to find the containing state
				owner = find(stack, StateKind)
				if owner == nil {
					traceback(fmt.Errorf("you must call Choice() within a State"))
				}
			}
		}
		if name == "" {
			name = fmt.Sprintf("choice_%d", len(model.elements))
		}
		qualifiedName := path.Join(owner.QualifiedName(), name)
		element := &vertex{
			element: element{kind: ChoiceKind, qualifiedName: qualifiedName},
		}
		model.members[qualifiedName] = element
		stack = append(stack, element)
		apply(model, stack, partialElements...)
		if len(element.transitions) == 0 {
			traceback(fmt.Errorf("you must define at least one transition for choice \"%s\"", qualifiedName))
		}
		if defaultTransition := get[*transition](model, element.transitions[len(element.transitions)-1]); defaultTransition != nil {
			if defaultTransition.Guard() != "" {
				traceback(fmt.Errorf("the last transition of choice state \"%s\" cannot have a guard", qualifiedName))
			}
		}
		return element
	}
}

// ShallowHistory creates a shallow history pseudostate within a composite state.
// If no history is available, any transitions defined on this pseudostate are used;
// otherwise the parent state's initial is used.
func ShallowHistory[T interface{ RedefinableElement | string }](elementOrName T, partialElements ...RedefinableElement) RedefinableElement {
	name := ""
	switch any(elementOrName).(type) {
	case string:
		name = any(elementOrName).(string)
	case RedefinableElement:
		partialElements = append([]RedefinableElement{any(elementOrName).(RedefinableElement)}, partialElements...)
	}
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, StateKind)
		if owner == nil {
			traceback(fmt.Errorf("you must call ShallowHistory() within a State"))
		}
		if name == "" {
			name = fmt.Sprintf("shallow_history_%d", len(model.elements))
		}
		qualifiedName := path.Join(owner.QualifiedName(), name)
		element := &vertex{
			element: element{kind: ShallowHistoryKind, qualifiedName: qualifiedName},
		}
		model.members[qualifiedName] = element
		stack = append(stack, element)
		apply(model, stack, partialElements...)
		return element
	}
}

// DeepHistory creates a deep history pseudostate within a composite state.
// If no history is available, any transitions defined on this pseudostate are used;
// otherwise the parent state's initial is used.
func DeepHistory[T interface{ RedefinableElement | string }](elementOrName T, partialElements ...RedefinableElement) RedefinableElement {
	name := ""
	switch any(elementOrName).(type) {
	case string:
		name = any(elementOrName).(string)
	case RedefinableElement:
		partialElements = append([]RedefinableElement{any(elementOrName).(RedefinableElement)}, partialElements...)
	}
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, StateKind)
		if owner == nil {
			traceback(fmt.Errorf("you must call DeepHistory() within a State"))
		}
		if name == "" {
			name = fmt.Sprintf("deep_history_%d", len(model.elements))
		}
		qualifiedName := path.Join(owner.QualifiedName(), name)
		element := &vertex{
			element: element{kind: DeepHistoryKind, qualifiedName: qualifiedName},
		}
		model.members[qualifiedName] = element
		stack = append(stack, element)
		apply(model, stack, partialElements...)
		return element
	}
}

// Entry defines an action to be executed when entering a state.
// The entry action is executed before any internal activities are started.
//
// Example:
//
//	hsm.Entry(func(ctx context.Context, hsm *MyHSM, event Event) {
//	    log.Printf("Entering state with event: %s", event.Name)
//	})
func Entry[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, StateKind).(*state)
		if owner == nil {
			traceback(fmt.Errorf("entry must be called within a State"))
		}
		for _, fn := range funcs {
			name := getFunctionName(fn)
			element := &behavior[T]{
				element:   element{kind: BehaviorKind, qualifiedName: path.Join(owner.QualifiedName(), name)},
				operation: fn,
			}
			model.members[element.QualifiedName()] = element
			owner.entry = append(owner.entry, element.QualifiedName())
		}
		return owner
	}
}

// Activity defines a long-running action that is executed while in a state.
// The activity is started after the entry action and stopped before the exit action.
//
// Example:
//
//	hsm.Activity(func(ctx context.Context, hsm *MyHSM, event Event) {
//	    for {
//	        select {
//	        case <-ctx.Done():
//	            return
//	        case <-time.After(time.Second):
//	            log.Println("Activity tick")
//	        }
//	    }
//	})
func Activity[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner, ok := find(stack, StateKind).(*state)
		if !ok {
			traceback(fmt.Errorf("activity must be called within a State"))
		}
		for _, fn := range funcs {
			name := getFunctionName(fn)
			element := &behavior[T]{
				element:   element{kind: ConcurrentKind, qualifiedName: path.Join(owner.QualifiedName(), name)},
				operation: fn,
			}
			model.members[element.QualifiedName()] = element
			owner.activities = append(owner.activities, element.QualifiedName())
		}
		return owner
	}
}

// Exit defines an action to be executed when exiting a state.
// The exit action is executed after any internal activities are stopped.
//
// Example:
//
//	hsm.Exit(func(ctx context.Context, hsm *MyHSM, event Event) {
//	    log.Printf("Exiting state with event: %s", event.Name)
//	})
func Exit[T Instance](funcs ...func(ctx context.Context, hsm T, event Event)) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner, ok := find(stack, StateKind).(*state)
		if !ok {
			traceback(fmt.Errorf("exit must be called within a State"))
		}
		for _, fn := range funcs {
			name := getFunctionName(fn)
			element := &behavior[T]{
				element:   element{kind: BehaviorKind, qualifiedName: path.Join(owner.QualifiedName(), name)},
				operation: fn,
			}
			model.members[element.QualifiedName()] = element
			owner.exit = append(owner.exit, element.QualifiedName())
		}
		return owner
	}
}

// On defines the events that can cause a transition.
// Multiple events can be specified for a single transition.
//
// Example:
//
//	hsm.Transition(
//	    hsm.On("start", "resume"),
//	    hsm.Source("idle"),
//	    hsm.Target("running")
//	)
func On[T interface{ *Event | Event }](events ...T) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, TransitionKind)
		if owner == nil {
			traceback(fmt.Errorf("trigger must be called within a Transition"))
		}
		transition := owner.(*transition)
		for _, eventOrName := range events {
			var name string
			var event *Event
			switch e := any(eventOrName).(type) {
			case Event:
				name = e.Name
				event = &e
			case *Event:
				name = e.Name
				event = e
			}
			transition.events = append(transition.events, name)
			registerEvent(traceback, model, event)
		}
		return owner
	}
}

// OnSet creates an attribute-change trigger for the given attribute name.
// It is the attribute-based equivalent of On(...) and is driven by Set.
func OnSet(name string) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, TransitionKind)
		if owner == nil {
			traceback(fmt.Errorf("OnSet() must be called within a Transition"))
		}
		if name == "" {
			traceback(fmt.Errorf("OnSet() requires a non-empty attribute name"))
		}
		qualifiedName := qualifyModelName(model.qualifiedName, name)
		transition := owner.(*transition)
		eventName := qualifiedName
		transition.events = append(transition.events, eventName)
		registerEvent(traceback, model, &Event{
			Kind:   ChangeEventKind,
			Name:   eventName,
			Source: qualifiedName,
		})
		if model.attributes == nil {
			model.attributes = map[string]*attribute{}
		}
		if _, exists := model.attributes[qualifiedName]; !exists {
			model.attributes[qualifiedName] = &attribute{name: qualifiedName}
		}
		return owner
	}
}

// OnCall creates a trigger for Call() invocations of the named operation.
func OnCall(name string) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, TransitionKind)
		if owner == nil {
			traceback(fmt.Errorf("OnCall() must be called within a Transition"))
		}
		if name == "" {
			traceback(fmt.Errorf("OnCall() requires a non-empty operation name"))
		}
		qualifiedName := qualifyModelName(model.qualifiedName, name)
		transition := owner.(*transition)
		eventName := qualifiedName
		transition.events = append(transition.events, eventName)
		registerEvent(traceback, model, &Event{
			Kind:   CallEventKind,
			Name:   eventName,
			Source: qualifiedName,
		})
		model.push(func(model *Model, stack []Element) Element {
			if model.operations == nil || model.operations[qualifiedName] == nil {
				traceback(fmt.Errorf("missing operation \"%s\" for OnCall()", qualifiedName))
			}
			return owner
		})
		return owner
	}
}

// After creates a time-based transition that occurs after a specified duration.
// The duration can be dynamically computed based on the state machine's context.
//
// Example:
//
//	hsm.Transition(
//	    hsm.After(func(ctx context.Context, hsm *MyHSM, event Event) time.Duration {
//	        return time.Second * 30
//	    }),
//	    hsm.Source("active"),
//	    hsm.Target("timeout")
//	)
func After[T Instance](expr func(ctx context.Context, hsm T, event Event) time.Duration) RedefinableElement {
	traceback := traceback()
	name := getFunctionName(expr)
	return func(model *Model, stack []Element) Element {
		owner, ok := find(stack, TransitionKind).(*transition)
		if !ok {
			traceback(fmt.Errorf("after must be called within a Transition"))
		}
		qualifiedName := path.Join(owner.QualifiedName(), name, strconv.Itoa(len(model.members)))
		// hash := crc32.ChecksumIEEE([]byte(qualifiedName))
		event := Event{
			Kind: TimeEventKind,
			// Id:   strconv.FormatUint(uint64(hash), 32),
			Name: qualifiedName,
		}
		owner.events = append(owner.events, qualifiedName)
		model.push(func(model *Model, stack []Element) Element {
			maybeSource, ok := model.members[owner.source]
			if !ok {
				traceback(fmt.Errorf("source \"%s\" for transition \"%s\" not found", owner.source, owner.QualifiedName()))
			}
			source, ok := maybeSource.(*state)
			if !ok {
				traceback(fmt.Errorf("after can only be used on transitions where the source is a State, not \"%s\"", maybeSource.QualifiedName()))
			}
			activity := &behavior[T]{
				element: element{kind: ConcurrentKind, qualifiedName: path.Join(source.QualifiedName(), "activity", qualifiedName)},
				operation: func(ctx context.Context, hsm T, _ Event) {
					duration := expr(ctx, hsm, event)
					if duration < 0 {
						return
					}
					timer := time.NewTimer(duration)
					select {
					case <-timer.C:
						timer.Stop()
						hsm.dispatch(hsm.Context(), event)
						return
					case <-ctx.Done():
						timer.Stop()
						return
					}
				},
			}
			model.members[activity.QualifiedName()] = activity
			source.activities = append(source.activities, activity.QualifiedName())
			return owner
		})
		return owner
	}
}

// Every schedules events to be processed on an interval.
//
// Example:
//
//	hsm.Every(func(ctx context.Context, hsm T, event Event) time.Duration {
//	    return time.Second * 30
//	})
func Every[T Instance](expr func(ctx context.Context, hsm T, event Event) time.Duration) RedefinableElement {
	traceback := traceback()
	name := getFunctionName(expr)
	return func(model *Model, stack []Element) Element {
		owner, ok := find(stack, TransitionKind).(*transition)
		if !ok {
			traceback(fmt.Errorf("after must be called within a Transition"))
		}
		qualifiedName := path.Join(owner.QualifiedName(), name, strconv.Itoa(len(model.members)))
		// hash := crc32.ChecksumIEEE([]byte(qualifiedName))
		event := Event{
			Kind: TimeEventKind,
			// Id:   strconv.FormatUint(uint64(hash), 32),
			Name: qualifiedName,
		}
		owner.events = append(owner.events, qualifiedName)
		model.push(func(model *Model, stack []Element) Element {
			maybeSource, ok := model.members[owner.source]
			if !ok {
				traceback(fmt.Errorf("source \"%s\" for transition \"%s\" not found", owner.source, owner.QualifiedName()))
			}
			source, ok := maybeSource.(*state)
			if !ok {
				traceback(fmt.Errorf("Every() can only be used on transitions where the source is a State, not \"%s\"", maybeSource.QualifiedName()))
			}
			activity := &behavior[T]{
				element: element{kind: ConcurrentKind, qualifiedName: path.Join(source.QualifiedName(), "activity", qualifiedName)},
				operation: func(ctx context.Context, hsm T, evt Event) {
					duration := expr(ctx, hsm, evt)
					if duration < 0 {
						return
					}
					timer := time.NewTimer(duration)
					defer timer.Stop()
					for {
						select {
						case <-timer.C:
							<-hsm.dispatch(hsm.Context(), event)
							timer.Reset(duration)
						case <-ctx.Done():
							return
						}
					}
				},
			}
			model.members[activity.QualifiedName()] = activity
			source.activities = append(source.activities, activity.QualifiedName())
			return owner
		})
		return owner
	}
}

func When[T Instance](expr func(ctx context.Context, hsm T, event Event) <-chan struct{}) RedefinableElement {
	traceback := traceback()
	name := getFunctionName(expr)
	return func(model *Model, stack []Element) Element {
		owner, ok := find(stack, TransitionKind).(*transition)
		if !ok {
			traceback(fmt.Errorf("when must be called within a Transition"))
		}
		qualifiedName := path.Join(owner.QualifiedName(), name, strconv.Itoa(len(model.members)))
		event := Event{
			Kind: TimeEventKind,
			Name: qualifiedName,
		}
		owner.events = append(owner.events, qualifiedName)
		model.push(func(model *Model, stack []Element) Element {
			maybeSource, ok := model.members[owner.source]
			if !ok {
				traceback(fmt.Errorf("source \"%s\" for transition \"%s\" not found", owner.source, owner.QualifiedName()))
			}
			source, ok := maybeSource.(*state)
			if !ok {
				traceback(fmt.Errorf("when can only be used on transitions where the source is a State, not \"%s\"", maybeSource.QualifiedName()))
			}
			activity := &behavior[T]{
				element: element{kind: ConcurrentKind, qualifiedName: path.Join(source.QualifiedName(), "activity", qualifiedName)},
				operation: func(ctx context.Context, hsm T, _ Event) {
					ch := expr(ctx, hsm, event)
					for {
						select {
						case <-ch:
							hsm.dispatch(hsm.Context(), event)
						case <-ctx.Done():
							return
						}
					}
				},
			}
			model.members[activity.QualifiedName()] = activity
			source.activities = append(source.activities, activity.QualifiedName())
			return owner
		})
		return owner
	}
}

// Final creates a final state that represents the completion of a composite state or the entire state machine.
// When a final state is entered, a completion event is generated.
//
// Example:
//
//	hsm.State("process",
//	    hsm.State("working"),
//	    hsm.Final("done"),
//	    hsm.Transition(
//	        hsm.Source("working"),
//	        hsm.Target("done")
//	    )
//	)
func Final(name string) RedefinableElement {
	traceback := traceback()
	return func(model *Model, stack []Element) Element {
		owner := find(stack, NamespaceKind)
		if owner == nil {
			traceback(fmt.Errorf("final \"%s\" must be called within Define() or State()", name))
		}
		state := &state{
			vertex: vertex{element: element{kind: FinalStateKind, qualifiedName: path.Join(owner.QualifiedName(), name)}, transitions: []string{}},
		}
		model.members[state.QualifiedName()] = state
		model.push(
			func(model *Model, stack []Element) Element {
				if len(state.transitions) > 0 {
					traceback(fmt.Errorf("final state \"%s\" cannot have transitions", state.QualifiedName()))
				}
				if len(state.activities) > 0 {
					traceback(fmt.Errorf("final state \"%s\" cannot have activities", state.QualifiedName()))
				}
				if len(state.entry) > 0 {
					traceback(fmt.Errorf("final state \"%s\" cannot have an entry action", state.QualifiedName()))
				}
				if len(state.exit) > 0 {
					traceback(fmt.Errorf("final state \"%s\" cannot have an exit action", state.QualifiedName()))
				}
				return state
			},
		)
		return state
	}
}

// Match provides a simple interface, handling basic cases directly
// and delegating complex matching to the match function.
func Match(value string, patterns ...string) bool {
	for _, pattern := range patterns {
		// fast path for exact match
		if pattern == value {
			return true
		}
		// fast path for pure wildcard match
		if pattern == "*" {
			return true
		}
		patternLen := len(pattern)
		// fast path for empty pattern
		if patternLen == 0 {
			return value == ""
		}
		// fast path for long strings with a pattern that ends with "*
		if pattern[patternLen-1] == '*' && strings.HasPrefix(value, pattern[:patternLen-1]) {
			return true
		}
		// parse the value and pattern to check for a match
		if parse(value, pattern) {
			return true
		}
	}
	return false
}

// parse implements wildcard matching using a goto-based iterative approach.
// It supports the '*' wildcard, which matches zero or more characters.
func parse(value, pattern string) bool {
	valueIndex, patternIndex := 0, 0
	valueLen, patternLen := len(value), len(pattern)
	// patternStarIndex: index of the last '*' encountered in the pattern p.
	// valueStarIndex: index in the value string v corresponding to the position *after* the characters matched by the last '*'.
	patternStarIndex, valueStarIndex := -1, -1

LOOP_START:
	// Check if the current pattern character is '*'
	if patternIndex < patternLen && pattern[patternIndex] == '*' {
		patternStarIndex = patternIndex // Remember the position of this '*'
		patternIndex++                  // Advance the pattern index past the '*'
		valueStarIndex = valueIndex     // Remember the value index where '*' matching might backtrack to
		// If '*' is the last character in the pattern, it matches the rest of the value
		if patternIndex == patternLen {
			return true
		}
		// Continue processing, effectively trying to match zero characters with '*' first
		goto LOOP_START
	}

	// Check if current characters match
	if valueIndex < valueLen && patternIndex < patternLen && pattern[patternIndex] == value[valueIndex] {
		valueIndex++    // Advance value index
		patternIndex++  // Advance pattern index
		goto LOOP_START // Continue matching the next characters
	}

	// Check if we have reached the end of both strings
	if valueIndex == valueLen && patternIndex == patternLen {
		return true // Both strings are exhausted, successful match
	}

	// Check if we reached the end of the value string, but the pattern string remains
	if valueIndex == valueLen && patternIndex < patternLen {
		// Consume any trailing '*' characters in the pattern
		for patternIndex < patternLen && pattern[patternIndex] == '*' {
			patternIndex++
		}
		// If the pattern is now exhausted, it's a match
		return patternIndex == patternLen
	}

	// Mismatch occurred, or end of pattern reached while value string still has characters.
	// Try backtracking if a '*' was previously encountered.
	if patternStarIndex != -1 {
		// Backtrack: Advance the value index associated with the last '*'
		valueStarIndex++
		// If the backtracking value index goes beyond the value string length, matching failed
		if valueStarIndex > valueLen {
			return false
		}
		valueIndex = valueStarIndex         // Reset the current value index to the new backtrack position
		patternIndex = patternStarIndex + 1 // Reset the pattern index to the character immediately after the last '*'
		goto LOOP_START                     // Retry matching from the new state
	}
	return false
}

type EventDetail struct {
	Event  string
	Target string
	Guard  bool
	Schema any
}

type Snapshot struct {
	ID            string
	QualifiedName string
	State         string
	Attributes    map[string]any
	QueueLen      int
	Events        []EventDetail
}

// HSM is the base type that should be embedded in custom state machine types.
// It provides the core state machine functionality.
//
// Example:
//
//	type MyHSM struct {
//	    hsm.HSM
//	    counter int
//	}

type instance = Instance

type HSM struct {
	instance
}

func (hsm *HSM) bind(instance Instance) {
	if hsm == nil || hsm.instance != nil {
		return
	}
	hsm.instance = instance
}

type ctx = context.Context
type active struct {
	ctx
	cancel  context.CancelFunc
	channel chan struct{}
}

type timeouts struct {
	activity time.Duration
}

type mutex struct {
	internal sync.RWMutex
	signal   atomic.Value
}

func (mutex *mutex) wLock() {
	mutex.internal.Lock()
	mutex.signal.Store(make(chan struct{}))
}

func (mutex *mutex) wUnlock() {
	mutex.internal.Unlock()
	signal := mutex.signal.Load().(chan struct{})
	close(signal)
}

func (mutex *mutex) wait() <-chan struct{} {
	signal := mutex.signal.Load().(chan struct{})
	return signal
}

func (mutex *mutex) tryLock() bool {
	if mutex.internal.TryLock() {
		mutex.signal.Store(make(chan struct{}))
		return true
	}
	return false
}

type after struct {
	entered    sync.Map
	exited     sync.Map
	dispatched sync.Map
	processed  sync.Map
	executed   sync.Map
}

// Group is a composite instance that forwards operations to multiple instances.
// It flattens nested groups and broadcasts events to all members.
type Group struct {
	instances []Instance
	after     after
	id        string
	context   context.Context
	cancel    context.CancelFunc
}

// NewGroup creates a new group from the provided instances.
// Nested groups are flattened.
func NewGroup(instances ...Instance) *Group {
	group := &Group{}
	for _, instance := range instances {
		if instance == nil {
			continue
		}
		if nested, ok := instance.(*Group); ok && nested != nil {
			if isStarted(nested) {
				panic(ErrAlreadyStarted)
			}
			group.instances = append(group.instances, nested.instances...)
			continue
		}
		if isStarted(instance) {
			panic(ErrAlreadyStarted)
		}
		group.instances = append(group.instances, instance)
	}
	return group
}

// Instances returns a snapshot of the group's instances.
func (group *Group) Instances() []Instance {
	if group == nil || len(group.instances) == 0 {
		return nil
	}
	return slices.Clone(group.instances)
}

func (group *Group) State() string {
	return ""
}

func (group *Group) Context() context.Context {
	if group == nil {
		return context.Background()
	}
	if group.context != nil {
		return group.context
	}
	if len(group.instances) == 0 {
		return context.Background()
	}
	return group.instances[0].Context()
}

func (group *Group) Get(name string) (any, bool) {
	if group == nil || len(group.instances) == 0 {
		return nil, false
	}
	return group.instances[0].Get(name)
}

func (group *Group) Set(ctx context.Context, name string, value any) <-chan struct{} {
	if group == nil || len(group.instances) == 0 {
		return closedChannel
	}
	return group.waitAll(ctx, func(instance Instance) <-chan struct{} {
		return instance.Set(ctx, name, value)
	})
}

func (group *Group) Call(ctx context.Context, name string, args ...any) (any, error) {
	if group == nil || len(group.instances) == 0 {
		return nil, ErrMissingHSM
	}
	return group.instances[0].Call(ctx, name, args...)
}

func (group *Group) channels() *after {
	if group == nil {
		return &after{}
	}
	return &group.after
}

func (group *Group) takeSnapshot() Snapshot {
	if group == nil {
		return Snapshot{}
	}
	return Snapshot{
		ID:            group.id,
		QualifiedName: "",
		State:         "",
		Attributes:    nil,
		QueueLen:      0,
		Events:        nil,
	}
}

func (group *Group) wait() <-chan struct{} {
	if group == nil || len(group.instances) == 0 {
		return closedChannel
	}
	return group.waitAll(context.Background(), func(instance Instance) <-chan struct{} {
		return instance.wait()
	})
}

func (group *Group) start(ctx context.Context, instance Instance, event *Event) {
	if group == nil || len(group.instances) == 0 {
		return
	}
	if isStarted(group) {
		panic(ErrAlreadyStarted)
	}
	for _, child := range group.instances {
		if isStarted(child) {
			panic(ErrAlreadyStarted)
		}
	}
	instances, ok := ctx.Value(Keys.Instances).(*sync.Map)
	if !ok || instances == nil {
		instances = &sync.Map{}
	}
	group.context, group.cancel = context.WithCancel(context.WithValue(context.WithValue(ctx, Keys.Instances, instances), Keys.HSM, group))
	for _, child := range group.instances {
		if child == nil {
			continue
		}
		child.start(group.context, child, event)
	}
}

func (group *Group) dispatch(ctx context.Context, event Event) <-chan struct{} {
	if group == nil || len(group.instances) == 0 {
		return closedChannel
	}
	if event.Kind == 0 {
		event.Kind = EventKind
	}
	if ch, ok := group.after.dispatched.LoadAndDelete(event.Name); ok {
		close(ch.(chan struct{}))
	}
	return group.waitAll(ctx, func(instance Instance) <-chan struct{} {
		return instance.dispatch(ctx, event)
	}, func() {
		if ch, ok := group.after.processed.LoadAndDelete(event.Name); ok {
			close(ch.(chan struct{}))
		}
	})
}

func (group *Group) bind(instance Instance) {}

func (group *Group) stop(ctx context.Context) <-chan struct{} {
	if group == nil || len(group.instances) == 0 {
		return closedChannel
	}
	return group.waitAll(ctx, func(instance Instance) <-chan struct{} {
		return instance.stop(ctx)
	}, func() {
		if group.cancel != nil {
			group.cancel()
		}
	})
}

func (group *Group) restart(ctx context.Context, maybeData ...any) <-chan struct{} {
	if group == nil || len(group.instances) == 0 {
		return closedChannel
	}
	return group.waitAll(ctx, func(instance Instance) <-chan struct{} {
		return instance.restart(ctx, maybeData...)
	})
}

func (group *Group) waitAll(ctx context.Context, request func(instance Instance) <-chan struct{}, onDone ...func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, instance := range group.instances {
			if instance == nil {
				continue
			}
			ch := request(instance)
			wg.Add(1)
			go func(ch <-chan struct{}) {
				defer wg.Done()
				if ch == nil {
					return
				}
				select {
				case <-ch:
				case <-ctx.Done():
				}
			}(ch)
		}
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		for _, callback := range onDone {
			if callback != nil {
				callback()
			}
		}
	}()
	return done
}

func isStarted(instance Instance) bool {
	if instance == nil {
		return false
	}
	ctx := instance.Context()
	if ctx == nil {
		return false
	}
	return ctx.Value(Keys.HSM) != nil
}

// Instance represents an active state machine instance that can process events and track state.
// It provides methods for event dispatch and state management.
type Instance interface {
	// State returns the current state's qualified name.
	State() string
	Context() context.Context
	Get(name string) (any, bool)
	Set(ctx context.Context, name string, value any) <-chan struct{}
	Call(ctx context.Context, name string, args ...any) (any, error)
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

type hsm[T Instance] struct {
	behavior[T]
	state          atomic.Value
	context        context.Context
	cancel         context.CancelFunc
	model          *Model
	active         map[string]*active
	queue          queue
	attributes     sync.Map
	historyShallow map[string]string
	historyDeep    map[string]string
	instance       T
	timeouts       timeouts
	processing     mutex
	after          after
}

// Config provides configuration options for state machine initialization.
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

type key[T any] struct{}

var Keys = struct {
	Instances key[*atomic.Pointer[[]Instance]]
	Owner     key[Instance]
	HSM       key[HSM]
}{
	Instances: key[*atomic.Pointer[[]Instance]]{},
	Owner:     key[Instance]{},
	HSM:       key[HSM]{},
}

// Started creates and starts a new state machine instance with the given model and configuration.
// The state machine will begin executing from its initial state.
//
// Example:
//
//	model := hsm.Define(...)
//	sm := hsm.Started(context.Background(), &MyHSM{}, &model, hsm.Config{
//	    Trace: func(ctx context.Context, step string, data ...any) (context.Context, func(...any)) {
//	        log.Printf("Step: %s, Data: %v", step, data)
//	        return ctx, func(...any) {}
//	    },
//	    Id: "my-hsm-1",
//	})
func Started[T Instance](ctx context.Context, sm T, model *Model, maybeConfig ...Config) T {
	new := New(sm, model, maybeConfig...)
	var data any
	if len(maybeConfig) > 0 {
		data = maybeConfig[0].Data
	}
	return Start(ctx, new, data)
}

func Start[T Instance](ctx context.Context, sm T, maybeData ...any) T {
	initialEvent := InitialEvent
	if len(maybeData) > 0 {
		initialEvent = initialEvent.WithData(maybeData[0])
	}
	sm.start(ctx, sm, &initialEvent)
	return sm
}

func New[T Instance](sm T, model *Model, maybeConfig ...Config) T {
	hsm := &hsm[T]{
		behavior: behavior[T]{
			element: element{
				kind: StateMachineKind,
			},
		},
		context:        context.Background(),
		cancel:         func() {},
		model:          model,
		instance:       sm,
		queue:          queue{},
		active:         map[string]*active{},
		historyShallow: map[string]string{},
		historyDeep:    map[string]string{},
	}
	hsm.state.Store(&model.state)
	if len(maybeConfig) > 0 {
		config := maybeConfig[0]
		hsm.timeouts.activity = config.ActivityTimeout
		hsm.behavior.qualifiedName = config.Name
		hsm.behavior.id = config.ID
	}
	if hsm.behavior.qualifiedName == "" {
		hsm.behavior.qualifiedName = model.QualifiedName()
	}
	if hsm.behavior.id == "" {
		hsm.behavior.id = fmt.Sprintf("%s_%s", Name(hsm), muid.Make().String())
	}
	if hsm.timeouts.activity == 0 {
		hsm.timeouts.activity = time.Millisecond
	}
	for name, attr := range model.attributes {
		if attr != nil && attr.hasDefault {
			hsm.attributes.Store(name, attr.defaultValue)
		}
	}
	hsm.behavior.operation = func(ctx context.Context, _ T, event Event) {
		hsm.state.Store(hsm.enter(ctx, &hsm.model.state, &event, true))
		hsm.process(ctx)
	}
	sm.bind(hsm)
	return sm
}

func (sm *hsm[T]) bind(instance Instance) {}

func (sm *hsm[T]) State() string {
	if sm == nil {
		return ""
	}
	state := sm.state.Load().(Element)
	if state == nil {
		return ""
	}
	return state.QualifiedName()
}

func (sm *hsm[T]) start(ctx context.Context, instance Instance, event *Event) {
	sm.processing.wLock()
	instances, ok := ctx.Value(Keys.Instances).(*sync.Map)
	if !ok {
		instances = &sync.Map{}
	}
	sm.context, sm.cancel = context.WithCancel(context.WithValue(context.WithValue(context.WithValue(ctx, Keys.Instances, instances), Keys.Owner, ctx.Value(Keys.HSM)), Keys.HSM, sm))
	instances.Store(sm.behavior.id, sm)
	sm.execute(sm.activate(sm.context, sm), &sm.behavior, event)
}

func (sm *hsm[T]) restart(ctx context.Context, maybeData ...any) <-chan struct{} {
	var data any
	if len(maybeData) > 0 {
		data = maybeData[0]
	}
	<-sm.stop(ctx)
	initialEvent := InitialEvent.WithData(data)
	sm.context = ctx
	sm.cancel = func() {}
	(*hsm[T])(sm).start(ctx, sm, &initialEvent)
	return sm.processing.wait()
}

func (sm *hsm[T]) wait() <-chan struct{} {
	return sm.processing.wait()
}

func (sm *hsm[T]) stop(ctx context.Context) <-chan struct{} {
	if sm == nil {
		return closedChannel
	}
	signal := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("panic in stop", "error", r)
			}
			close(signal)
		}()
		sm.processing.wLock()

		var ok bool
		state := sm.state.Load().(Element)
		for state != nil {
			select {
			case <-ctx.Done():
				return
			default:
				sm.exit(ctx, state, &FinalEvent)
				state, ok = sm.model.members[state.Owner()]
				if ok {
					sm.state.Store(state)
					continue
				}
			}
			break
		}
		sm.cancel()
		clear(sm.active)
		clear(sm.historyShallow)
		clear(sm.historyDeep)
		if instances, ok := sm.context.Value(Keys.Instances).(*sync.Map); ok {
			instances.Delete(sm.behavior.id)
		}

		sm.processing.wUnlock()
	}()
	return signal
}

func (sm *hsm[T]) Context() context.Context {
	if sm == nil {
		return nil
	}
	return sm.context
}

func (sm *hsm[T]) Get(name string) (any, bool) {
	if sm == nil {
		return nil, false
	}
	qualifiedName := qualifyModelName(sm.model.qualifiedName, name)
	return sm.attributes.Load(qualifiedName)
}

func (sm *hsm[T]) Set(ctx context.Context, name string, value any) <-chan struct{} {
	return sm.setAttribute(ctx, name, value, true)
}

func (sm *hsm[T]) setAttribute(ctx context.Context, name string, value any, emit bool) <-chan struct{} {
	if sm == nil {
		return closedChannel
	}
	if ctx == nil {
		ctx = sm.context
	}
	qualifiedName := qualifyModelName(sm.model.qualifiedName, name)
	old, exists := sm.attributes.Load(qualifiedName)
	sm.attributes.Store(qualifiedName, value)
	if !emit {
		return closedChannel
	}
	if exists && reflect.DeepEqual(old, value) {
		return closedChannel
	}
	event := Event{
		Kind:   ChangeEventKind,
		Name:   qualifiedName,
		Source: qualifiedName,
		Data: AttributeChange{
			Name: qualifiedName,
			Old:  old,
			New:  value,
		},
	}
	return sm.dispatch(ctx, event)
}

func (sm *hsm[T]) Call(ctx context.Context, name string, args ...any) (any, error) {
	if sm == nil {
		return nil, ErrNilHSM
	}
	if ctx == nil {
		ctx = sm.context
	}
	if name == "" {
		return nil, ErrInvalidOperation
	}
	qualifiedName := qualifyModelName(sm.model.qualifiedName, name)
	op := sm.model.operations[qualifiedName]
	if op == nil {
		return nil, ErrMissingOperation
	}
	event := Event{
		Kind:   CallEventKind,
		Name:   qualifiedName,
		Source: qualifiedName,
		Data: CallData{
			Name: qualifiedName,
			Args: args,
		},
	}
	sm.dispatch(ctx, event)
	return sm.invokeOperation(op, ctx, args...)
}

func (sm *hsm[T]) invokeOperation(op *operationDef, ctx context.Context, args ...any) (any, error) {
	if op == nil {
		return nil, ErrMissingOperation
	}
	fnType := op.fnType
	fnValue := op.fnValue
	if !fnValue.IsValid() || fnType.Kind() != reflect.Func {
		return nil, ErrInvalidOperation
	}
	instanceValue := reflect.ValueOf(sm.instance)
	ctxValue := reflect.ValueOf(ctx)
	type candidate struct {
		useCtx      bool
		useInstance bool
	}
	candidates := []candidate{
		{useCtx: true, useInstance: true},
		{useCtx: true, useInstance: false},
		{useCtx: false, useInstance: true},
		{useCtx: false, useInstance: false},
	}

	var callArgs []reflect.Value
	matched := false
	for _, c := range candidates {
		callArgs = callArgs[:0]
		argIndex := 0
		if c.useCtx {
			if argIndex >= fnType.NumIn() {
				continue
			}
			paramType := fnType.In(argIndex)
			if !ctxValue.Type().AssignableTo(paramType) && !ctxValue.Type().ConvertibleTo(paramType) {
				continue
			}
			if ctxValue.Type().AssignableTo(paramType) {
				callArgs = append(callArgs, ctxValue)
			} else {
				callArgs = append(callArgs, ctxValue.Convert(paramType))
			}
			argIndex++
		}
		if c.useInstance {
			if argIndex >= fnType.NumIn() {
				continue
			}
			paramType := fnType.In(argIndex)
			switch {
			case instanceValue.Type().AssignableTo(paramType):
				callArgs = append(callArgs, instanceValue)
			case instanceValue.Type().ConvertibleTo(paramType):
				callArgs = append(callArgs, instanceValue.Convert(paramType))
			case instanceValue.Kind() == reflect.Pointer && instanceValue.Elem().IsValid() && instanceValue.Elem().Type().AssignableTo(paramType):
				callArgs = append(callArgs, instanceValue.Elem())
			case instanceValue.Kind() == reflect.Pointer && instanceValue.Elem().IsValid() && instanceValue.Elem().Type().ConvertibleTo(paramType):
				callArgs = append(callArgs, instanceValue.Elem().Convert(paramType))
			default:
				continue
			}
			argIndex++
		}
		remainingParams := fnType.NumIn() - argIndex
		if !fnType.IsVariadic() {
			if remainingParams != len(args) {
				continue
			}
		} else {
			if len(args) < remainingParams-1 {
				continue
			}
		}
		validArgs := true
		for i := 0; i < len(args); i++ {
			paramIndex := argIndex + i
			var paramType reflect.Type
			if fnType.IsVariadic() && paramIndex >= fnType.NumIn()-1 {
				paramType = fnType.In(fnType.NumIn() - 1).Elem()
			} else if paramIndex < fnType.NumIn() {
				paramType = fnType.In(paramIndex)
			} else {
				validArgs = false
				break
			}
			value := reflect.ValueOf(args[i])
			if !value.IsValid() {
				if paramType.Kind() == reflect.Interface || paramType.Kind() == reflect.Pointer || paramType.Kind() == reflect.Slice || paramType.Kind() == reflect.Map || paramType.Kind() == reflect.Func || paramType.Kind() == reflect.Chan {
					callArgs = append(callArgs, reflect.Zero(paramType))
					continue
				}
				validArgs = false
				break
			}
			if value.Type().AssignableTo(paramType) {
				callArgs = append(callArgs, value)
				continue
			}
			if value.Type().ConvertibleTo(paramType) {
				callArgs = append(callArgs, value.Convert(paramType))
				continue
			}
			validArgs = false
			break
		}
		if !validArgs {
			continue
		}
		matched = true
		break
	}

	if !matched {
		return nil, ErrInvalidOperation
	}

	results := fnValue.Call(callArgs)
	switch len(results) {
	case 0:
		return nil, nil
	case 1:
		if err, ok := results[0].Interface().(error); ok {
			return nil, err
		}
		return results[0].Interface(), nil
	default:
		last := results[len(results)-1].Interface()
		if err, ok := last.(error); ok {
			if len(results) == 2 {
				return results[0].Interface(), err
			}
			return results[:len(results)-1], err
		}
		return results[0].Interface(), nil
	}
}

func (sm *hsm[T]) channels() *after {
	if sm == nil {
		return nil
	}
	return &sm.after
}

func (sm *hsm[T]) activate(ctx context.Context, element Element) *active {
	if element == nil {
		return nil
	}
	qualifiedName := element.QualifiedName()
	maybeActive, ok := sm.active[qualifiedName]
	if !ok {
		maybeActive = &active{
			channel: make(chan struct{}, 1),
		}
		sm.active[qualifiedName] = maybeActive
	}
	maybeActive.ctx, maybeActive.cancel = context.WithCancel(ctx)
	return maybeActive
}

func (sm *hsm[T]) executeAll(ctx context.Context, names []string, event *Event) {
	for _, qualifiedName := range names {
		if behavior := get[*behavior[T]](sm.model, qualifiedName); behavior != nil {
			sm.execute(ctx, behavior, event)
		}
	}
}

func (sm *hsm[T]) enter(ctx context.Context, element Element, event *Event, defaultEntry bool) Element {
	if sm == nil {
		return nil
	}
	switch element.Kind() {
	case StateKind:
		state := element.(*state)
		sm.recordHistory(state.QualifiedName())
		for _, entry := range state.entry {
			if entry := get[*behavior[T]](sm.model, entry); entry != nil {
				sm.execute(ctx, entry, event)
			}
		}
		if len(state.activities) > 0 {
			sm.executeAll(ctx, state.activities, event)
		}
		if !defaultEntry || state.initial == "" {
			return state
		}
		if initial := get[*vertex](sm.model, state.initial); initial != nil {
			if len(initial.transitions) > 0 {
				if transition := get[*transition](sm.model, initial.transitions[0]); transition != nil {
					return sm.transition(ctx, state, transition, event)
				}
			}
		}
		return state
	case ChoiceKind:
		choiceVertex := element.(*vertex)
		for _, qualifiedName := range choiceVertex.transitions {
			if transition := get[*transition](sm.model, qualifiedName); transition != nil {
				if constraint := get[*constraint[T]](sm.model, transition.Guard()); constraint != nil {
					if !sm.evaluate(ctx, constraint, event) {
						continue
					}
				}
				return sm.transition(ctx, element, transition, event)
			}
		}
	case ShallowHistoryKind, DeepHistoryKind:
		historyVertex := element.(*vertex)
		parent := element.Owner()
		if parent == "" {
			return element
		}
		resolved := sm.resolveHistory(parent, element.Kind())
		if resolved != "" && !IsAncestor(parent, resolved) {
			resolved = ""
		}
		if resolved == "" {
			if next := sm.followHistoryDefault(ctx, historyVertex, event); next != nil {
				return next
			}
			if parentState := get[*state](sm.model, parent); parentState != nil && parentState.initial != "" {
				if initialVertex := get[*vertex](sm.model, parentState.initial); initialVertex != nil {
					if len(initialVertex.transitions) > 0 {
						if transition := get[*transition](sm.model, initialVertex.transitions[0]); transition != nil {
							return sm.transition(ctx, parentState, transition, event)
						}
					}
				}
			}
			return element
		}
		enterPath := buildEnterPath(parent, resolved)
		for i, entering := range enterPath {
			next, ok := sm.model.members[entering]
			if !ok {
				return nil
			}
			defaultEntry := false
			if element.Kind() == ShallowHistoryKind && i == len(enterPath)-1 {
				defaultEntry = true
			}
			current := sm.enter(ctx, next, event, defaultEntry)
			if i == len(enterPath)-1 {
				return current
			}
		}
		return element
	case FinalStateKind:
		if element.Owner() == sm.model.qualifiedName {
			sm.cancel()
		}
		return element
	}
	return nil
}

func (sm *hsm[T]) exit(ctx context.Context, element Element, event *Event) {
	if sm == nil || element == nil {
		return
	}
	if state, ok := element.(*state); ok {
		// if len(state.activities) > 0 {
		// 	sm.terminateAll(ctx, state.activities)
		// }
		for _, activity := range state.activities {
			if activity := get[*behavior[T]](sm.model, activity); activity != nil {
				sm.terminate(ctx, activity)
			}
		}
		for _, exit := range state.exit {
			if exit := get[*behavior[T]](sm.model, exit); exit != nil {
				sm.execute(ctx, exit, event)
			}
		}
	}

}

func (sm *hsm[T]) recordHistory(stateName string) {
	if sm == nil || stateName == "" {
		return
	}
	child := stateName
	parent := path.Dir(child)
	for parent != "" && parent != "." {
		if parent == "/" {
			break
		}
		if element := sm.model.members[parent]; element != nil && kind.Is(element.Kind(), StateKind) {
			sm.historyDeep[parent] = stateName
			sm.historyShallow[parent] = child
		}
		if parent == sm.model.qualifiedName {
			break
		}
		next := path.Dir(parent)
		if next == parent {
			break
		}
		child = parent
		parent = next
	}
}

func (sm *hsm[T]) resolveHistory(parent string, historyKind uint64) string {
	switch historyKind {
	case ShallowHistoryKind:
		return sm.historyShallow[parent]
	case DeepHistoryKind:
		return sm.historyDeep[parent]
	default:
		return ""
	}
}

func (sm *hsm[T]) followHistoryDefault(ctx context.Context, historyVertex *vertex, event *Event) Element {
	if historyVertex == nil {
		return nil
	}
	for _, qualifiedName := range historyVertex.transitions {
		if transition := get[*transition](sm.model, qualifiedName); transition != nil {
			if constraint := get[*constraint[T]](sm.model, transition.Guard()); constraint != nil {
				if !sm.evaluate(ctx, constraint, event) {
					continue
				}
			}
			return sm.transition(ctx, historyVertex, transition, event)
		}
	}
	return nil
}

func buildEnterPath(parent, target string) []string {
	if parent == "" || target == "" {
		return nil
	}
	if parent == target {
		return nil
	}
	enter := []string{}
	current := target
	for current != "" && current != parent {
		enter = append([]string{current}, enter...)
		next := path.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return enter
}

func cleanup[T Instance](ctx context.Context, sm *hsm[T], element Element) {
	if r := recover(); r != nil {
		slog.Error("hsm: panic in concurrent behavior", "error", r, "stack", string(debug.Stack()))
		go sm.dispatch(ctx, ErrorEvent.WithData(fmt.Errorf("panic in concurrent behavior %s: %s", element.QualifiedName(), r)))
	}
	if ch, ok := sm.after.executed.LoadAndDelete(element.QualifiedName()); ok {
		close(ch.(chan struct{}))
	}
}

func (sm *hsm[T]) execute(ctx context.Context, element *behavior[T], event *Event) {
	if sm == nil || element == nil {
		return
	}
	switch element.Kind() {
	case ConcurrentKind:
		ctx := sm.activate(sm.context, element)
		go func(ctx *active, event Event) {
			defer cleanup(ctx, sm, element)
			element.operation(ctx, sm.instance, event)
			ctx.channel <- struct{}{}
		}(ctx, *event)
	default:
		defer cleanup(ctx, sm, element)
		element.operation(ctx, sm.instance, *event)
	}

}

func (sm *hsm[T]) evaluate(ctx context.Context, guard *constraint[T], event *Event) bool {
	if sm == nil || guard == nil || guard.expression == nil {
		return true
	}
	return guard.expression(
		ctx,
		sm.instance,
		*event,
	)
}

func (sm *hsm[T]) transition(ctx context.Context, current Element, transition *transition, event *Event) Element {
	if sm == nil {
		return nil
	}
	path, ok := transition.paths[current.QualifiedName()]
	if !ok {
		return nil
	}
	for _, exiting := range path.exit {
		current, ok = sm.model.members[exiting]
		if !ok {
			return nil
		}
		sm.exit(ctx, current, event)
		if ch, ok := sm.after.exited.LoadAndDelete(exiting); ok {
			close(ch.(chan struct{}))
		}
	}
	for _, effect := range transition.effect {
		if effect := get[*behavior[T]](sm.model, effect); effect != nil {
			sm.execute(ctx, effect, event)
		}
	}
	if kind.Is(transition.kind, InternalKind) {
		return current
	}
	for _, entering := range path.enter {
		next, ok := sm.model.members[entering]
		if !ok {
			return nil
		}
		defaultEntry := entering == transition.target
		current = sm.enter(ctx, next, event, defaultEntry)
		if ch, ok := sm.after.entered.LoadAndDelete(entering); ok {
			close(ch.(chan struct{}))
		}
		if defaultEntry {
			return current
		}
	}
	current, ok = sm.model.members[transition.target]
	if !ok {
		return nil
	}
	return current
}

func (sm *hsm[T]) terminate(ctx context.Context, element Element) {
	if sm == nil || element == nil {
		return
	}
	maybeActive, ok := sm.active[element.QualifiedName()]
	if !ok {
		return
	}
	maybeActive.cancel()
	select {
	case <-maybeActive.channel:
	case <-time.After(sm.timeouts.activity):
		go sm.dispatch(ctx, ErrorEvent.WithData(fmt.Errorf("terminate timeout: %s", element.QualifiedName())))
	}

}

func (sm *hsm[T]) process(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("hsm: panic while processing event in state machine: %v\n\n%s", r, string(debug.Stack()))
			slog.Error("hsm: panic while processing event in state machine", "error", err)
			go sm.dispatch(ctx, ErrorEvent.WithData(err))
		}
		sm.processing.wUnlock()
	}()
	if sm == nil {
		return
	}
	var deferred []Event
	event, ok := sm.queue.pop()
	for ok {
		transitionTaken, isDeferred := sm.processEvent(ctx, &event)
		if isDeferred {
			deferred = append(deferred, event)
			event, _ = sm.queue.pop()
			continue
		}
		if transitionTaken && len(deferred) > 0 {
			sm.queue.push(deferred...)
			deferred = nil
		}
		event, ok = sm.queue.pop()
	}
	sm.queue.push(deferred...)
}

func (sm *hsm[T]) processEvent(ctx context.Context, event *Event) (transitionTaken bool, deferred bool) {
	if sm == nil || event == nil {
		return false, false
	}
	if event.Kind == 0 {
		event.Kind = EventKind
	}
	if event.ID == "" {
		event.ID = muid.MakeString()
	}
	currentState := sm.state.Load().(Element)
	currentQualifiedName := currentState.QualifiedName()

	// Check if event is deferred using O(1) lookup
	if deferredSet, ok := sm.model.DeferredMap[currentQualifiedName]; ok {
		if _, isDeferred := deferredSet[event.Name]; isDeferred {
			return false, true
		}
	}

	// Direct O(1) lookup for transitions - no hierarchy walking needed
	if transitions, ok := sm.model.TransitionMap[currentQualifiedName][event.Name]; ok {
		for _, transition := range transitions {
			if guard := get[*constraint[T]](sm.model, transition.Guard()); guard != nil {
				if !sm.evaluate(ctx, guard, event) {
					continue
				}
			}
			state := sm.transition(ctx, currentState, transition, event)
			if state != nil {
				sm.state.Store(state)
				transitionTaken = true
			}
			break
		}
	}

	// If no specific transition was taken, check for AnyEvent transitions
	// AnyEvent transitions are only taken when no explicit event match with passing guard exists
	if !transitionTaken && event.Name != AnyEvent.Name {
		if anyTransitions, ok := sm.model.TransitionMap[currentQualifiedName][AnyEvent.Name]; ok {
			for _, transition := range anyTransitions {
				if guard := get[*constraint[T]](sm.model, transition.Guard()); guard != nil {
					if !sm.evaluate(ctx, guard, event) {
						continue
					}
				}
				state := sm.transition(ctx, currentState, transition, event)
				if state != nil {
					sm.state.Store(state)
					transitionTaken = true
				}
				break
			}
		}
	}

	if ch, ok := sm.after.processed.LoadAndDelete(event.Name); ok {
		close(ch.(chan struct{}))
	}
	return transitionTaken, false
}

func (sm *hsm[T]) takeSnapshot() Snapshot {
	if sm == nil {
		return Snapshot{}
	}
	state, ok := sm.state.Load().(Element)
	if !ok {
		state = sm.model
	}

	var attributes map[string]any
	sm.attributes.Range(func(key, value any) bool {
		name, ok := key.(string)
		if !ok {
			return true
		}
		if attributes == nil {
			attributes = map[string]any{}
		}
		attributes[name] = value
		return true
	})

	var events []EventDetail
	currentQualifiedName := state.QualifiedName()

	// Get transitions for the current state
	if stateTransitions, ok := sm.model.TransitionMap[currentQualifiedName]; ok {
		for eventName, transitions := range stateTransitions {
			// Check if this event is EventKind
			if event, exists := sm.model.events[eventName]; exists && event.Kind == EventKind {
				for _, transition := range transitions {
					// Check if transition has a guard
					hasGuard := false
					if guardName := transition.Guard(); guardName != "" {
						if guard := get[*constraint[T]](sm.model, guardName); guard != nil {
							hasGuard = true
						}
					}

					events = append(events, EventDetail{
						Event:  eventName,
						Target: transition.Target(),
						Guard:  hasGuard,
						Schema: event.Schema,
					})
				}
			}
		}
	}

	return Snapshot{
		ID:            sm.behavior.id,
		QualifiedName: sm.behavior.qualifiedName,
		State:         state.QualifiedName(),
		Attributes:    attributes,
		QueueLen:      sm.queue.len(),
		Events:        events,
	}
}

func (sm *hsm[T]) dispatch(ctx context.Context, event Event) <-chan struct{} {
	if sm == nil {
		return closedChannel
	}
	state := sm.state.Load().(Element)
	if state == nil {
		return closedChannel
	}
	if event.Kind == 0 {
		event.Kind = EventKind
	}
	sm.queue.push(event)
	if sm.processing.tryLock() {
		go sm.process(context.WithoutCancel(ctx))
	}
	if ch, ok := sm.after.dispatched.LoadAndDelete(event.Name); ok {
		close(ch.(chan struct{}))
	}
	return sm.processing.wait()
}

// Dispatch sends an event to a specific state machine instance.
// Returns a channel that closes when the event has been fully processed.
//
// Example:
//
//	sm := hsm.Start(...)
//	done := sm.Dispatch(hsm.Event{Name: "start"})
//	<-done // Wait for event processing to complete
func Dispatch[T context.Context](ctx T, hsm Instance, event Event) <-chan struct{} {
	if hsm != nil {
		return hsm.dispatch(ctx, event)
	}
	// get the hsm from the context
	if hsm, ok := FromContext(ctx); ok {
		// dispatch the event to the hsm
		return hsm.dispatch(ctx, event)
	}
	return closedChannel
}

// Get reads an attribute value from the given state machine or from context.
func Get(ctx context.Context, hsm Instance, name string) (any, bool) {
	if hsm != nil {
		return hsm.Get(name)
	}
	if hsm, ok := FromContext(ctx); ok {
		return hsm.Get(name)
	}
	return nil, false
}

// Set updates an attribute value and emits an OnSet change event.
func Set(ctx context.Context, hsm Instance, name string, value any) <-chan struct{} {
	if hsm != nil {
		return hsm.Set(ctx, name, value)
	}
	if hsm, ok := FromContext(ctx); ok {
		return hsm.Set(ctx, name, value)
	}
	return closedChannel
}

// Call dispatches an OnCall event and invokes the named operation.
func Call(ctx context.Context, hsm Instance, name string, args ...any) (any, error) {
	if hsm != nil {
		return hsm.Call(ctx, name, args...)
	}
	if hsm, ok := FromContext(ctx); ok {
		return hsm.Call(ctx, name, args...)
	}
	return nil, ErrMissingHSM
}

// DispatchAll sends an event to all state machine instances in the current context.
// Returns a channel that closes when all instances have processed the event.
// DispatchAll sends an event to all state machine instances in the current context.
// Returns a channel that closes when all instances have processed the event.
//
// Example:
//
//	sm1 := hsm.Start(...)
//	sm2 := hsm.Start(...)
//	done := hsm.DispatchAll(context.Background(), hsm.Event{Name: "globalEvent"})
//	<-done // Wait for all instances to process the event
func DispatchAll(ctx context.Context, event Event) <-chan struct{} {
	return DispatchTo(ctx, event)
}

func DispatchTo(ctx context.Context, event Event, maybeIds ...string) <-chan struct{} {
	instances, ok := ctx.Value(Keys.Instances).(*sync.Map)
	if !ok || instances == nil {
		return closedChannel
	}
	signal := make(chan struct{})
	go func(signal chan struct{}) {
		defer close(signal)
		signals := make(map[string]<-chan struct{})
		instances.Range(func(key, value any) bool {
			snapshot := value.(Instance).takeSnapshot()
			if len(maybeIds) == 0 || Match(snapshot.ID, maybeIds...) {
				signals[key.(string)] = value.(Instance).dispatch(ctx, event)
			}
			return true
		})
		for len(signals) > 0 {
			for i, ch := range signals {
				select {
				case <-ch:
					delete(signals, i)
				case <-ctx.Done():
					return
				}
			}
		}
	}(signal)
	return signal
}

// AfterProcess returns a channel that closes when event processing completes.
// If an event is provided, the channel closes after that specific event is processed.
// If no event is provided, the channel closes after the next processing cycle completes.
// This is useful for synchronizing with state machine execution in tests or
// when coordinating external operations with state transitions.
func AfterProcess(ctx context.Context, hsm Instance, maybeEvent ...Event) <-chan struct{} {
	if len(maybeEvent) > 0 {
		ch, _ := hsm.channels().processed.LoadOrStore(maybeEvent[0].Name, make(chan struct{}))
		return ch.(chan struct{})
	} else {
		return hsm.wait()
	}
}

// AfterDispatch returns a channel that closes when the specified event is dispatched.
// Unlike AfterProcess, this signals when the event is added to the queue, not when
// processing completes. Useful for confirming event delivery before processing begins.
func AfterDispatch(ctx context.Context, hsm Instance, event Event) <-chan struct{} {
	ch, _ := hsm.channels().dispatched.LoadOrStore(event.Name, make(chan struct{}))
	return ch.(chan struct{})
}

// AfterEntry returns a channel that closes when the specified state is entered.
// The state parameter should be the fully qualified state path (e.g., "/parent/child").
// Useful for waiting until a particular state becomes active.
func AfterEntry(ctx context.Context, hsm Instance, state string) <-chan struct{} {
	ch, _ := hsm.channels().entered.LoadOrStore(state, make(chan struct{}))
	return ch.(chan struct{})
}

// AfterExit returns a channel that closes when the specified state is exited.
// The state parameter should be the fully qualified state path (e.g., "/parent/child").
// Useful for waiting until a particular state is no longer active.
func AfterExit(ctx context.Context, hsm Instance, state string) <-chan struct{} {
	ch, _ := hsm.channels().exited.LoadOrStore(state, make(chan struct{}))
	return ch.(chan struct{})
}

// AfterExecuted returns a channel that closes when the specified state's
// do-activity has completed execution. The state parameter should be the
// fully qualified state path. Useful for waiting until a state's background
// activity finishes.
func AfterExecuted(ctx context.Context, hsm Instance, state string) <-chan struct{} {
	ch, _ := hsm.channels().executed.LoadOrStore(state, make(chan struct{}))
	return ch.(chan struct{})
}

// FromContext retrieves a state machine instance from a context.
// Returns the instance and a boolean indicating whether it was found.
//
// Example:
//
//	if sm, ok := hsm.FromContext(ctx); ok {
//	    log.Printf("Current state: %s", sm.State())
//	}
func FromContext(ctx context.Context) (Instance, bool) {
	hsm, ok := ctx.Value(Keys.HSM).(Instance)
	if ok {
		return hsm, true
	}
	return nil, false
}

// InstancesFromContext retrieves all state machine instances from a context.
// Returns a slice of instances and a boolean indicating whether any were found.
// This is useful when multiple state machines share a context and you need
// to access or iterate over all of them.
func InstancesFromContext(ctx context.Context) ([]Instance, bool) {
	instancesPointer, ok := ctx.Value(Keys.Instances).(*sync.Map)
	if !ok || instancesPointer == nil {
		return nil, false
	}
	instances := make([]Instance, 0)
	instancesPointer.Range(func(key, value any) bool {
		instances = append(instances, value.(Instance))
		return true
	})
	return instances, true
}

// Stop gracefully stops a state machine instance.
// It cancels any running activities and prevents further event processing.
//
// Example:
//
//	sm := hsm.Start(...)
//	// ... use state machine ...
//	hsm.Stop(sm)
func Stop(ctx context.Context, hsm Instance) <-chan struct{} {
	return hsm.stop(ctx)
}

// Restart stops a state machine and restarts it from the initial state.
// Optional data can be passed to reinitialize the state machine's data field.
// Returns a channel that closes when the restart completes.
func Restart(ctx context.Context, hsm Instance, maybeData ...any) <-chan struct{} {
	return hsm.restart(ctx, maybeData...)
}

// ID returns the unique identifier of a state machine instance.
// The ID is assigned when the state machine is created and remains
// constant throughout its lifecycle.
func ID(hsm Instance) string {
	snapshot := hsm.takeSnapshot()
	return snapshot.ID
}

// QualifiedName returns the fully qualified name of a state machine instance.
// For nested state machines, this includes the parent path (e.g., "/parent/child").
// For top-level state machines, this is typically just the name with a leading slash.
func QualifiedName(hsm Instance) string {
	snapshot := hsm.takeSnapshot()
	return snapshot.QualifiedName
}

// Name returns the simple name of a state machine instance (without path prefix).
// This extracts the base name from the qualified name, e.g., "child" from "/parent/child".
func Name(hsm Instance) string {
	snapshot := hsm.takeSnapshot()
	return path.Base(snapshot.QualifiedName)
}

// TakeSnapshot captures the current state of a state machine instance.
// The returned Snapshot contains the ID, qualified name, current state,
// data, and other attributes representing the instance at this moment.
// Useful for debugging, logging, or persisting state machine state.
func TakeSnapshot(ctx context.Context, hsm Instance) Snapshot {
	return hsm.takeSnapshot()
}
