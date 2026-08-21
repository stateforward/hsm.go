// Package hsm provides a powerful hierarchical state machine (HSM) implementation for Go.
//
// # Overview
//
// It enables modeling complex state-driven systems with features like hierarchical states,
// entry/exit actions, guard conditions, and event-driven transitions. The implementation
// follows the Stateforward HSM DSL/runtime contract, ensuring consistency across
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
//	        hsm.On("moveToBar"),
//	        hsm.Source("foo"),
//	        hsm.Target("bar"),
//	    ),
//	    hsm.Initial(hsm.Target("foo")),
//	)
//
//	// Start the state machine
//	sm := hsm.Started(context.Background(), &MyHSM{}, &model)
//	<-hsm.Dispatch(context.Background(), sm, hsm.Event{Name: "moveToBar"})
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
	"sort"
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
	// SubmachineStateKind represents a state that composes a reusable child
	// machine under the containing state boundary.
	SubmachineStateKind = kind.Make(StateKind)
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
	// AttributeKind represents a model-level data slot.
	AttributeKind = kind.Make(ElementKind)
	// OperationKind represents a model-level callable operation.
	OperationKind = kind.Make(ElementKind)
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
	// ObservationKind represents a model observation hook.
	ObservationKind = kind.Make(ElementKind)
	// EntryPointKind represents a connection point used to enter a submachine
	// through a named transient vertex.
	EntryPointKind = kind.Make(PseudostateKind)
	// ExitPointKind represents a connection point used to leave a submachine
	// through a named transient vertex.
	ExitPointKind = kind.Make(PseudostateKind)
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

// MakeKind creates a new kind using the canonical HSM kind implementation.
func MakeKind(bases ...uint64) uint64 {
	return kind.Make(bases...)
}

// IsKind reports whether k matches any of the provided base kinds.
func IsKind(k uint64, bases ...uint64) bool {
	return kind.Is(k, bases...)
}

type stringLike interface {
	~string
}

type redefinableOrString interface {
	RedefinableElement | ~string
}

var stringValueType = reflect.TypeFor[string]()

func normalizeRedefinableOrString[T redefinableOrString](value T) (string, RedefinableElement, bool) {
	if element, ok := any(value).(RedefinableElement); ok {
		return "", element, true
	}
	if name, ok := stringLikeValue(value); ok {
		return name, nil, false
	}
	panic(fmt.Sprintf("expected string-like or RedefinableElement, got %T", value))
}

func stringLikeValue(value any) (string, bool) {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.String {
		return "", false
	}
	return reflected.Convert(stringValueType).Interface().(string), true
}

func requireRedefinableElement(value any) (RedefinableElement, bool) {
	element, ok := value.(RedefinableElement)
	if !ok {
		return nil, false
	}
	if element == nil {
		panic(fmt.Sprintf("expected string-like or RedefinableElement, got %T", value))
	}
	return element, true
}

// AttributeType returns a reflect.Type token for explicit Attribute declarations.
func AttributeType[T any]() reflect.Type {
	return reflect.TypeFor[T]()
}

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
	// ErrUnknownAttribute is returned when a runtime attribute name is not defined.
	ErrUnknownAttribute = errors.New("unknown attribute")
	// ErrInvalidAttributeType is returned when a runtime attribute value has the wrong type.
	ErrInvalidAttributeType = errors.New("invalid attribute type")
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

// RedefinableElement is a function type that modifies a Model by adding or updating elements.
// It's used to build the state machine structure in a declarative way.
type RedefinableElement = func(model *Model, stack []Element) Element

/******* Vertex *******/

type vertex struct {
	element
	transitions []string
}

func (vertex *vertex) Transitions() []string {
	return slices.Clone(vertex.transitions)
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
}

func (transition *transition) Guard() string {
	return transition.guard
}

func (transition *transition) Effect() []string {
	return slices.Clone(transition.effect)
}

func (transition *transition) Events() []string {
	return slices.Clone(transition.events)
}

func (transition *transition) hasEvent(name string) bool {
	for _, event := range transition.events {
		if event == name {
			return true
		}
	}
	return false
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
	operation    OperationFunc[T]
	operationRef string
	operationAny any
}

func (behavior *behavior[T]) wrapObservation(observer func(context.Context, Instance, Event)) {
	if behavior == nil || observer == nil {
		return
	}
	operation := behavior.operation
	qualifiedName := behavior.QualifiedName()
	behavior.operationAny = nil
	behavior.operation = func(ctx context.Context, hsm T, event Event) {
		observer(ctx, hsm, observationEvent(qualifiedName, "behavior", event))
		if operation != nil {
			operation(ctx, hsm, event)
		}
	}
}

/******* Constraint *******/

type constraint[T Instance] struct {
	element
	expression    ExpressionFunc[T]
	operationRef  string
	expressionAny any
}

/******* Attribute & OperationFunc *******/

type attribute struct {
	element
	name         string
	typ          reflect.Type
	defaultValue any
	hasDefault   bool
}

func (attr *attribute) valueType() reflect.Type {
	if attr == nil {
		return nil
	}
	return attr.typ
}

type operationDef struct {
	element
	name    string
	fn      any
	fnValue reflect.Value
	fnType  reflect.Type
}

type operationInvoker func(context.Context, ...any) (any, error)

type operationInvokerCacheKey struct {
	name     string
	argCount int
	argTypes [4]reflect.Type
	overflow string
}

type operationCallSpec struct {
	fnValue     reflect.Value
	fnType      reflect.Type
	useCtx      bool
	useInstance bool
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

// ObservationData is the payload for hsm/observation events.
type ObservationData struct {
	Event      Event
	Occurrence string
	Time       time.Time
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
		Source: e.Source,
		Target: e.Target,
		Data:   data,
		Schema: e.Schema,
	}
}

func (e Event) startupClone() Event {
	e.Data = cloneMetadataValue(e.Data)
	e.Schema = cloneMetadataValue(e.Schema)
	return e
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
	return slices.Clone(state.entry)
}

func (state *state) Activities() []string {
	return slices.Clone(state.activities)
}

func (state *state) Exit() []string {
	return slices.Clone(state.exit)
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
	validator  ModelValidator
	finalizer  ModelFinalizer
	observers  []observation
	rebaseRefs map[string]string
}

// FinalizedModel is the runtime-ready form of a declarative Model.
// It owns derived dispatch indexes used by runtime instances.
type FinalizedModel struct {
	*Model
	transitionMap   map[string]map[string][]*transition
	deferredMap     map[string]map[string]string
	transitionPaths map[*transition]map[string]paths
	historyPaths    map[string]map[string][]string
	historyTargets  map[historyTargetKey]map[string]string
}

type historyTargetKey struct {
	stateName string
	skipOwner string
}

// ComposableModel is a model value that can provide a submachine body.
type ComposableModel interface {
	Model | *Model | FinalizedModel | *FinalizedModel
}

// ModelValidator validates a model after all elements have been applied.
type ModelValidator interface {
	Validate(model *Model)
}

// ModelValidatorFunc adapts a function to ModelValidator.
type ModelValidatorFunc func(model *Model)

func (fn ModelValidatorFunc) Validate(model *Model) {
	if fn != nil {
		fn(model)
	}
}

// DefaultModelValidator runs the package's built-in model validation.
type DefaultModelValidator struct{}

func (DefaultModelValidator) Validate(model *Model) {
	validateModel(model)
}

// ModelFinalizer prepares a validated model for runtime use.
type ModelFinalizer interface {
	Finalize(model *Model) *FinalizedModel
}

// ModelFinalizerFunc adapts a function to ModelFinalizer.
type ModelFinalizerFunc func(model *Model) *FinalizedModel

func (fn ModelFinalizerFunc) Finalize(model *Model) *FinalizedModel {
	if fn == nil {
		return nil
	}
	return fn(model)
}

// DefaultModelFinalizer builds the package's runtime dispatch indexes.
type DefaultModelFinalizer struct{}

func (DefaultModelFinalizer) Finalize(model *Model) *FinalizedModel {
	return finalizeModel(model)
}

type observableBehavior interface {
	Element
	wrapObservation(func(context.Context, Instance, Event))
}

type observation struct {
	element
	operation OperationFunc[Instance]
	targets   []string
}

func (model *Model) Members() map[string]Element {
	if model == nil || model.members == nil {
		return nil
	}
	members := make(map[string]Element, len(model.members))
	for name, member := range model.members {
		members[name] = cloneElement(member)
	}
	return members
}

func cloneModel(model *Model) *Model {
	if model == nil {
		return nil
	}
	clone := *model
	clone.state = *cloneState(&model.state)
	clone.members = make(map[string]Element, len(model.members))
	clone.events = make(map[string]*Event, len(model.events))
	clone.attributes = map[string]*attribute{}
	clone.operations = map[string]*operationDef{}
	clone.elements = slices.Clone(model.elements)
	clone.observers = slices.Clone(model.observers)
	for index := range clone.observers {
		clone.observers[index].targets = slices.Clone(model.observers[index].targets)
	}
	if model.rebaseRefs != nil {
		clone.rebaseRefs = make(map[string]string, len(model.rebaseRefs))
		for name, target := range model.rebaseRefs {
			clone.rebaseRefs[name] = target
		}
	}
	for name, event := range model.events {
		clone.events[name] = cloneEvent(event)
	}
	for name, member := range model.members {
		cloned := cloneElement(member)
		if name == model.qualifiedName {
			if state, ok := cloned.(*state); ok {
				clone.state = *state
			}
			cloned = &clone.state
		}
		clone.members[name] = cloned
	}
	for name, member := range clone.members {
		switch typed := member.(type) {
		case *attribute:
			clone.attributes[name] = typed
		case *operationDef:
			clone.operations[name] = typed
		}
	}
	for name, attr := range model.attributes {
		if clone.attributes[name] == nil {
			cloned := cloneAttribute(attr)
			clone.attributes[name] = cloned
			clone.members[name] = cloned
		}
	}
	for name, op := range model.operations {
		if clone.operations[name] == nil {
			cloned := cloneOperationDef(op)
			clone.operations[name] = cloned
			clone.members[name] = cloned
		}
	}
	return &clone
}

func cloneElement(member Element) Element {
	switch typed := member.(type) {
	case *state:
		return cloneState(typed)
	case *vertex:
		return cloneVertexPointer(typed)
	case *transition:
		return cloneTransition(typed)
	case *behavior[Instance]:
		return cloneBehavior(typed)
	case *constraint[Instance]:
		return cloneConstraint(typed)
	case *attribute:
		return cloneAttribute(typed)
	case *operationDef:
		return cloneOperationDef(typed)
	case *observation:
		return cloneObservation(typed)
	default:
		return member
	}
}

func cloneState(source *state) *state {
	if source == nil {
		return nil
	}
	clone := *source
	clone.vertex = cloneVertex(source.vertex)
	clone.entry = slices.Clone(source.entry)
	clone.exit = slices.Clone(source.exit)
	clone.activities = slices.Clone(source.activities)
	clone.deferred = slices.Clone(source.deferred)
	return &clone
}

func cloneVertexPointer(source *vertex) *vertex {
	if source == nil {
		return nil
	}
	clone := cloneVertex(*source)
	return &clone
}

func cloneVertex(source vertex) vertex {
	clone := source
	clone.transitions = slices.Clone(source.transitions)
	return clone
}

func cloneTransition(source *transition) *transition {
	if source == nil {
		return nil
	}
	clone := *source
	clone.effect = slices.Clone(source.effect)
	clone.events = slices.Clone(source.events)
	return &clone
}

func cloneBehavior(source *behavior[Instance]) *behavior[Instance] {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func cloneConstraint(source *constraint[Instance]) *constraint[Instance] {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func cloneAttribute(source *attribute) *attribute {
	if source == nil {
		return nil
	}
	clone := *source
	clone.defaultValue = cloneMetadataValue(source.defaultValue)
	return &clone
}

func cloneOperationDef(source *operationDef) *operationDef {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func cloneObservation(source *observation) *observation {
	if source == nil {
		return nil
	}
	clone := *source
	clone.targets = slices.Clone(source.targets)
	return &clone
}

func cloneEvent(source *Event) *Event {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Data = cloneMetadataValue(source.Data)
	clone.Schema = cloneMetadataValue(source.Schema)
	return &clone
}

func compositionElements[M ComposableModel](machine M) (string, []RedefinableElement) {
	switch typed := any(machine).(type) {
	case Model:
		return typed.QualifiedName(), typed.elements
	case *Model:
		if typed != nil {
			return typed.QualifiedName(), typed.elements
		}
	case FinalizedModel:
		if typed.Model != nil {
			return typed.QualifiedName(), typed.elements
		}
	case *FinalizedModel:
		if typed != nil && typed.Model != nil {
			return typed.QualifiedName(), typed.elements
		}
	}
	return "", nil
}

// TransitionSnapshots returns a stable snapshot of model transitions.
func (model *Model) TransitionSnapshots() []TransitionSnapshot {
	if model == nil {
		return nil
	}
	transitions := make([]TransitionSnapshot, 0)
	for _, member := range model.members {
		transition, ok := member.(*transition)
		if !ok {
			continue
		}
		transitions = append(transitions, TransitionSnapshot{
			Name:   transition.QualifiedName(),
			Kind:   transition.Kind(),
			Source: transition.Source(),
			Target: transition.Target(),
			Events: slices.Clone(transition.Events()),
			Guard:  transition.Guard() != "",
		})
	}
	sort.Slice(transitions, func(i, j int) bool {
		return transitions[i].Name < transitions[j].Name
	})
	return transitions
}

func (model *Model) push(partial RedefinableElement) {
	model.elements = append(model.elements, partial)
}

type redefinableModel struct {
	name      string
	elements  []RedefinableElement
	validator ModelValidator
	finalizer ModelFinalizer
}

func cloneMetadataValue(value any) any {
	if value == nil {
		return nil
	}
	cloned := cloneMetadataReflect(reflect.ValueOf(value), map[cloneVisit]reflect.Value{})
	if !cloned.IsValid() {
		return nil
	}
	return cloned.Interface()
}

type cloneVisit struct {
	typ reflect.Type
	ptr uintptr
}

func cloneMetadataReflect(value reflect.Value, seen map[cloneVisit]reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneMetadataReflect(value.Elem(), seen)
		if !cloned.IsValid() {
			return reflect.Zero(value.Type())
		}
		if cloned.Type().AssignableTo(value.Type()) {
			return cloned
		}
		result := reflect.New(value.Type()).Elem()
		if cloned.Type().AssignableTo(result.Type()) {
			result.Set(cloned)
			return result
		}
		if cloned.Type().Implements(value.Type()) {
			result.Set(cloned)
			return result
		}
		return value
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if cloned, ok := seen[visit]; ok {
			return cloned
		}
		cloned := reflect.New(value.Type().Elem())
		seen[visit] = cloned
		cloned.Elem().Set(cloneMetadataReflect(value.Elem(), seen))
		return cloned
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if cloned, ok := seen[visit]; ok {
			return cloned
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		seen[visit] = cloned
		iter := value.MapRange()
		for iter.Next() {
			cloned.SetMapIndex(cloneMetadataReflect(iter.Key(), seen), cloneMetadataReflect(iter.Value(), seen))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		visit := cloneVisit{typ: value.Type(), ptr: value.Pointer()}
		if visit.ptr != 0 {
			if cloned, ok := seen[visit]; ok {
				return cloned
			}
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		if visit.ptr != 0 {
			seen[visit] = cloned
		}
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneMetadataReflect(value.Index(i), seen))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			cloned.Index(i).Set(cloneMetadataReflect(value.Index(i), seen))
		}
		return cloned
	case reflect.Struct:
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if cloned.Field(i).CanSet() {
				cloned.Field(i).Set(cloneMetadataReflect(value.Field(i), seen))
			}
		}
		return cloned
	default:
		return value
	}
}

// Built-in event types and special duration constants used by the HSM runtime.
var (
	// InitialEvent is the completion event dispatched when a state machine starts.
	// It triggers the initial transition from the initial pseudostate to the first active state.
	InitialEvent = Event{
		Name: "hsm/initial",
		Kind: CompletionEventKind,
	}
	// ErrorEvent is dispatched when an error occurs during state machine execution,
	// such as panics in concurrent behaviors or timeout errors. Use On(ErrorEvent) to handle errors.
	ErrorEvent = Event{
		Name: "hsm/error",
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
		Name: "hsm/final",
		Kind: CompletionEventKind,
	}
	// ObservationEvent is emitted to observation hooks for observed behaviors and transitions.
	ObservationEvent = Event{
		Name: "hsm/observation",
		Kind: EventKind,
	}
	// InfiniteDuration represents an unbounded duration for timeout operations.
	// Use this when a behavior should run indefinitely without timing out.
	InfiniteDuration = time.Duration(-1)
)

const internalEventIDPrefix = "\x00hsm:"

var internalEventIDCounter atomic.Uint64

func nextInternalEventID() string {
	return internalEventIDPrefix + strconv.FormatUint(internalEventIDCounter.Add(1), 36)
}

func isInternalEventID(id string) bool {
	return strings.HasPrefix(id, internalEventIDPrefix)
}

func observationEvent(source, occurrence string, observed Event) Event {
	event := ObservationEvent
	event.Source = source
	event.Target = observed.Target
	event.Schema = observed.Schema
	event.Data = ObservationData{
		Event:      observed,
		Occurrence: occurrence,
		Time:       time.Now(),
	}
	return event
}

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

var completedChannel = func() chan error {
	done := make(chan error)
	close(done)
	return done
}()

// Completion is a one-shot receive-only channel that yields the runtime error,
// if any, when the operation completes. A nil value means the operation
// succeeded.
type Completion <-chan error

func completedCompletion() Completion {
	return completedChannel
}

func failedCompletion(err error) Completion {
	done := make(chan error, 1)
	done <- err
	close(done)
	return done
}

func completionAfter(wait <-chan struct{}) Completion {
	if wait == nil {
		return completedCompletion()
	}
	done := make(chan error)
	go func() {
		<-wait
		close(done)
	}()
	return done
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type queue struct {
	mutex  sync.RWMutex
	events []Event
}

var empty = Event{}

// Queue provides injectable buffering for regular state machine events.
// Push, Pop, and Len are synchronous hooks: each operation must complete and
// return its event/availability/error directly to the runtime.
// Completion events are always stored in the runtime-owned LIFO side of Queue
// and are selected before the configurable regular-event queue.
type Queue struct {
	Push func(ctx context.Context, event Event) error
	Pop  func(ctx context.Context) (Event, bool, error)
	Len  func(ctx context.Context) (int, error)
	lifo *queue
}

func newQueue() Queue {
	q := &queue{}
	return Queue{
		Push: q.push,
		Pop:  q.pop,
		Len:  q.len,
		lifo: &queue{},
	}
}

func (q Queue) withDefaults() Queue {
	if q.isZero() {
		return newQueue()
	}
	q = q.validate()
	q.lifo = &queue{}
	return q
}

func (q Queue) validate() Queue {
	if q.Push == nil || q.Pop == nil || q.Len == nil {
		panic(ErrInvalidOperation)
	}
	return q
}

func (q Queue) isZero() bool {
	return q.Push == nil && q.Pop == nil && q.Len == nil
}

func (q *queue) len(context.Context) (int, error) {
	q.mutex.RLock()
	defer q.mutex.RUnlock()
	return len(q.events), nil
}

func (q *queue) pop(context.Context) (Event, bool, error) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if len(q.events) == 0 {
		return empty, false, nil
	}
	events := q.events
	event := events[0]
	events[0] = empty
	if len(events) == 1 {
		q.events = events[:0]
	} else if remaining := events[1:]; cap(remaining) > 1024 && len(remaining)*4 < cap(remaining) {
		compact := make([]Event, len(remaining))
		copy(compact, remaining)
		q.events = compact
	} else {
		q.events = events[1:]
	}
	return event, true, nil
}

func (q *queue) push(_ context.Context, event Event) error {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	q.events = append(q.events, event)
	return nil
}

func (q *Queue) len(ctx context.Context) (int, error) {
	lifoLen := 0
	if q.lifo != nil {
		lifoLen, _ = q.lifo.len(ctx)
	}
	fifoLen, err := q.Len(ctx)
	return lifoLen + fifoLen, err
}

func (q *Queue) pop(ctx context.Context) (Event, bool, error) {
	if q.lifo != nil {
		q.lifo.mutex.Lock()
		if len(q.lifo.events) > 0 {
			event := q.lifo.events[len(q.lifo.events)-1]
			q.lifo.events = q.lifo.events[:len(q.lifo.events)-1]
			q.lifo.mutex.Unlock()
			return event, true, nil
		}
		q.lifo.mutex.Unlock()
	}
	return q.Pop(ctx)
}

func (q *Queue) push(ctx context.Context, event Event) error {
	if kind.Is(event.Kind, CompletionEventKind) {
		if q.lifo == nil {
			q.lifo = &queue{}
		}
		q.lifo.mutex.Lock()
		q.lifo.events = append(q.lifo.events, event)
		q.lifo.mutex.Unlock()
		return nil
	}
	return q.Push(ctx, event)
}

type EventSnapshot struct {
	Name   string
	Kind   uint64 `json:"-"`
	Target string
	Guard  bool
	// Schema is copied from model metadata when the snapshot is captured.
	// Mutable map, slice, array, pointer, interface, and exported struct fields
	// are recursively copied where Go reflection permits.
	Schema any
}

type TransitionSnapshot struct {
	Name   string
	Kind   uint64 `json:"-"`
	Source string
	Target string
	Events []string
	Guard  bool
}

type Snapshot struct {
	ID            string
	QualifiedName string
	State         string
	// Attributes is a fresh map whose values are copied from runtime storage.
	// Mutating this map, or mutable values reachable only through it, does not
	// change the running instance.
	Attributes map[string]any
	QueueLen   int
	// Events is a fresh slice for this snapshot. Event schemas follow
	// EventSnapshot.Schema copy semantics.
	Events      []EventSnapshot
	Transitions []TransitionSnapshot
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

func (hsm *HSM) invokeOperationReference(ctx context.Context, name string, args ...any) (any, error) {
	if hsm == nil || hsm.instance == nil {
		return nil, ErrNilHSM
	}
	invoker, ok := hsm.instance.(interface {
		invokeOperationReference(context.Context, string, ...any) (any, error)
	})
	if !ok {
		return nil, ErrMissingOperation
	}
	return invoker.invokeOperationReference(ctx, name, args...)
}

func (hsm *HSM) takeSnapshot() Snapshot {
	if hsm == nil || hsm.instance == nil {
		return Snapshot{}
	}
	snapshotter, ok := hsm.instance.(interface {
		takeSnapshot() Snapshot
	})
	if !ok {
		return Snapshot{}
	}
	return snapshotter.takeSnapshot()
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

// Clock provides injectable timer functions used by the state machine runtime.
// Nil fields fall back to DefaultClock.
type Clock struct {
	After    func(time.Duration) <-chan time.Time
	NewTimer func(time.Duration) *time.Timer
}

// DefaultClock is used when Config.Clock does not override timer behavior.
var DefaultClock = Clock{
	After:    time.After,
	NewTimer: time.NewTimer,
}

func (clock Clock) withDefaults() Clock {
	defaultClock := DefaultClock
	if defaultClock.After == nil {
		defaultClock.After = time.After
	}
	if defaultClock.NewTimer == nil {
		defaultClock.NewTimer = time.NewTimer
	}
	if clock.After == nil {
		clock.After = defaultClock.After
	}
	if clock.NewTimer == nil {
		clock.NewTimer = defaultClock.NewTimer
	}
	return clock
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
	signal := mutex.signal.Load().(chan struct{})
	close(signal)
	mutex.internal.Unlock()
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

type processingDrain struct {
	mutex     sync.Mutex
	scheduled bool
	waiters   []chan error
	ctx       context.Context
	eventID   string
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
			group.instances = append(group.instances, nested.instances...)
			continue
		}
		group.instances = append(group.instances, instance)
	}
	return group
}

// MakeGroup creates a new group from the provided instances.
// If the first argument is a string, it is used as the group ID.
func MakeGroup(values ...any) *Group {
	var id string
	if len(values) > 0 {
		if groupID, ok := values[0].(string); ok {
			id = groupID
			values = values[1:]
		}
	}
	instances := make([]Instance, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		instance, ok := value.(Instance)
		if !ok {
			panic(fmt.Sprintf("expected hsm.Instance, got %T", value))
		}
		instances = append(instances, instance)
	}
	group := NewGroup(instances...)
	group.id = id
	return group
}

// Instances returns a snapshot of the group's instances.
func (group *Group) Instances() []Instance {
	if group == nil || len(group.instances) == 0 {
		return nil
	}
	return slices.Clone(group.instances)
}

// States returns the current state path for each grouped instance in group order.
func (group *Group) States() []string {
	if group == nil || len(group.instances) == 0 {
		return nil
	}
	states := make([]string, 0, len(group.instances))
	for _, instance := range group.instances {
		if instance == nil {
			continue
		}
		states = append(states, instance.State())
	}
	return states
}

// Snapshots captures one snapshot per grouped instance in group order.
func (group *Group) Snapshots() []Snapshot {
	return group.takeSnapshots()
}

func (group *Group) State() string {
	states := group.States()
	if len(states) == 0 {
		return ""
	}
	return strings.Join(states, "\n")
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

func (group *Group) get(name string) (any, bool) {
	if group == nil || len(group.instances) == 0 {
		return nil, false
	}
	return Get(context.Background(), group.instances[0], name)
}

func (group *Group) set(ctx context.Context, name string, value any) Completion {
	if group == nil || len(group.instances) == 0 {
		return completedCompletion()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waiters := make([]<-chan error, 0, len(group.instances))
	for _, instance := range group.instances {
		if instance == nil {
			continue
		}
		waiters = append(waiters, instance.set(ctx, name, value))
	}
	return waitForAllCompletions(ctx, waiters)
}

func (group *Group) call(ctx context.Context, name string, args ...any) (any, error) {
	if group == nil || len(group.instances) == 0 {
		return nil, ErrMissingHSM
	}
	return Call(ctx, group.instances[0], name, args...)
}

func (group *Group) channels() *after {
	if group == nil {
		return &after{}
	}
	return &group.after
}

func (group *Group) takeSnapshot() []Snapshot {
	return group.takeSnapshots()
}

func (group *Group) takeSnapshots() []Snapshot {
	if group == nil || len(group.instances) == 0 {
		return nil
	}
	snapshots := make([]Snapshot, 0, len(group.instances))
	for _, instance := range group.instances {
		snapshot, ok := instanceSnapshot(instance)
		if !ok {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

func (group *Group) queueLen(snapshots []Snapshot) int {
	queueLen := 0
	for _, snapshot := range snapshots {
		queueLen += snapshot.QueueLen
	}
	return queueLen
}

func (group *Group) wait() <-chan struct{} {
	if group == nil || len(group.instances) == 0 {
		return closedChannel
	}
	waiters := make([]<-chan struct{}, 0, len(group.instances))
	for _, instance := range group.instances {
		if instance != nil {
			waiters = append(waiters, instance.wait())
		}
	}
	return waitForAll(context.Background(), waiters)
}

func (group *Group) start(ctx context.Context, event *Event) {
	if group == nil || len(group.instances) == 0 {
		return
	}
	if group.context != nil && group.context.Err() == nil {
		panic(ErrAlreadyStarted)
	}
	instances, ok := ctx.Value(Keys.Instances).(*sync.Map)
	if !ok || instances == nil {
		instances = &sync.Map{}
	}
	group.context, group.cancel = context.WithCancel(context.WithValue(context.WithValue(ctx, Keys.Instances, instances), Keys.HSM, group))
	for _, child := range group.instances {
		if child == nil || isStarted(child) {
			continue
		}
		startEvent := event.startupClone()
		child.start(group.context, &startEvent)
	}
}

func (group *Group) dispatch(ctx context.Context, event Event) Completion {
	if group == nil || len(group.instances) == 0 {
		return completedCompletion()
	}
	if event.Kind == 0 {
		event.Kind = EventKind
	}
	if ch, ok := group.after.dispatched.LoadAndDelete(event.Name); ok {
		close(ch.(chan struct{}))
	}
	source := ""
	if current, ok := FromContext(ctx); ok {
		source = ID(current)
	}
	return group.waitAll(ctx, func(instance Instance) Completion {
		if !isStarted(instance) {
			return completedCompletion()
		}
		targetedEvent := eventForTarget(event, source, ID(instance))
		return instance.dispatch(ctx, targetedEvent)
	}, func() {
		if ch, ok := group.after.processed.LoadAndDelete(event.Name); ok {
			close(ch.(chan struct{}))
		}
	})
}

func (group *Group) bind(instance Instance) {}

func (group *Group) qualifiedName() string {
	if group == nil {
		return ""
	}
	return group.id
}

func (group *Group) stop(ctx context.Context) Completion {
	if group == nil || len(group.instances) == 0 {
		return completedCompletion()
	}
	return group.waitAll(ctx, func(instance Instance) Completion {
		return instance.stop(ctx)
	}, func() {
		if group.cancel != nil {
			group.cancel()
		}
	})
}

func (group *Group) restart(ctx context.Context, maybeData ...any) Completion {
	if group == nil || len(group.instances) == 0 {
		return completedCompletion()
	}
	if !isStarted(group) {
		return completedCompletion()
	}
	stopCtx, startCtx := restartContexts(ctx, group)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		select {
		case err := <-group.stop(stopCtx):
			if err != nil {
				done <- err
				return
			}
		case <-stopCtx.Done():
			return
		}
		if stopCtx.Err() != nil {
			return
		}
		initialEvent := InitialEvent
		if len(maybeData) > 0 {
			initialEvent = initialEvent.WithData(maybeData[0])
		}
		group.start(startCtx, &initialEvent)
	}()
	return done
}

func (group *Group) Clock() Clock {
	return DefaultClock
}

func (group *Group) waitAll(ctx context.Context, request func(instance Instance) Completion, onDone ...func()) Completion {
	waiters := make([]<-chan error, 0, len(group.instances))
	for _, instance := range group.instances {
		if instance == nil {
			continue
		}
		waiters = append(waiters, request(instance))
	}
	return waitForAllCompletions(ctx, waiters, onDone...)
}

func waitForAll(ctx context.Context, waiters []<-chan struct{}, onDone ...func()) <-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, ch := range waiters {
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

func waitForAllCompletions(ctx context.Context, waiters []<-chan error, onDone ...func()) Completion {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		var firstErr error
		for _, ch := range waiters {
			if ch == nil {
				continue
			}
			select {
			case err := <-ch:
				if firstErr == nil {
					firstErr = err
				}
			case <-ctx.Done():
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		for _, callback := range onDone {
			if callback != nil {
				callback()
			}
		}
		if firstErr != nil {
			done <- firstErr
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
	if ctx.Err() != nil {
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
	get(name string) (any, bool)
	set(ctx context.Context, name string, value any) Completion
	call(ctx context.Context, name string, args ...any) (any, error)
	// non exported
	channels() *after
	wait() <-chan struct{}
	start(ctx context.Context, event *Event)
	dispatch(ctx context.Context, event Event) Completion
	bind(instance Instance)
	stop(ctx context.Context) Completion
	restart(ctx context.Context, maybeData ...any) Completion
	Clock() Clock
}

type hsm[T Instance] struct {
	behavior[T]
	state          atomic.Value
	context        context.Context
	cancel         context.CancelFunc
	model          *FinalizedModel
	active         map[string]*active
	queue          Queue
	attributes     sync.Map
	historyShallow map[string]string
	historyDeep    map[string]string
	instance       T
	behaviors      map[string]*behavior[T]
	constraints    map[string]*constraint[T]
	operations     sync.Map
	clock          Clock
	timeouts       timeouts
	processing     mutex
	drain          processingDrain
	after          after
}

// Config provides configuration options for state machine initialization.
type Config struct {
	// ID is a unique identifier for the state machine instance.
	ID string
	// ActivityTimeout is the timeout for the state activity to terminate.
	ActivityTimeout time.Duration
	// Clock overrides the timer functions used by the state machine runtime.
	Clock Clock
	// Queue overrides the event buffer used by the state machine runtime.
	Queue Queue
	// Name is the name of the state machine.
	Name string
	// Data to be passed during initialization
	Data any
}

type key[T any] struct{}

type runtimeContextKey string

const processingContextKey runtimeContextKey = "processing"

var Keys = struct {
	Instances key[*sync.Map]
	Owner     key[Instance]
	HSM       key[HSM]
}{
	Instances: key[*sync.Map]{},
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
//	    ID: "my-hsm-1",
//	    ActivityTimeout: 5 * time.Second,
//	})
func Started[T Instance](ctx context.Context, sm T, model *FinalizedModel, maybeConfig ...Config) T {
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
	sm.start(ctx, &initialEvent)
	return sm
}

func New[T Instance](sm T, model *FinalizedModel, maybeConfig ...Config) T {
	if model == nil || model.Model == nil || model.transitionMap == nil || model.deferredMap == nil || model.transitionPaths == nil || model.historyPaths == nil || model.historyTargets == nil {
		panic(fmt.Errorf("hsm: finalized model is required; create models with Define or Redefine"))
	}
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
		queue:          newQueue(),
		active:         map[string]*active{},
		historyShallow: map[string]string{},
		historyDeep:    map[string]string{},
		behaviors:      map[string]*behavior[T]{},
		constraints:    map[string]*constraint[T]{},
		clock:          DefaultClock.withDefaults(),
	}
	hsm.state.Store(&model.state)
	if len(maybeConfig) > 0 {
		config := maybeConfig[0]
		hsm.timeouts.activity = config.ActivityTimeout
		hsm.clock = config.Clock.withDefaults()
		hsm.queue = config.Queue.withDefaults()
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
	hsm.resetAttributes()
	hsm.behavior.operation = func(ctx context.Context, _ T, event Event) {
		if state := hsm.enter(ctx, &hsm.model.state, &event, true); state != nil {
			hsm.state.Store(state)
		}
		hsm.process(ctx, "")
	}
	hsm.bindModelCaches()
	sm.bind(hsm)
	return sm
}

func (sm *hsm[T]) bind(instance Instance) {}

func (sm *hsm[T]) resetAttributes() {
	if sm == nil || sm.model == nil {
		return
	}
	sm.attributes.Range(func(key, _ any) bool {
		sm.attributes.Delete(key)
		return true
	})
	for name, attr := range sm.model.attributes {
		if attr != nil && attr.hasDefault {
			sm.attributes.Store(name, cloneMetadataValue(attr.defaultValue))
		}
	}
}

func (sm *hsm[T]) id() string {
	if sm == nil {
		return ""
	}
	return sm.behavior.id
}

func (sm *hsm[T]) qualifiedName() string {
	if sm == nil {
		return ""
	}
	return sm.behavior.qualifiedName
}

func (hsm *HSM) id() string {
	if hsm == nil || hsm.instance == nil {
		return ""
	}
	return instanceID(hsm.instance)
}

func (hsm *HSM) qualifiedName() string {
	if hsm == nil || hsm.instance == nil {
		return ""
	}
	return instanceQualifiedName(hsm.instance)
}

func instanceID(instance Instance) string {
	if instance == nil {
		return ""
	}
	if group, ok := instance.(*Group); ok {
		return group.id
	}
	if provider, ok := instance.(interface{ id() string }); ok {
		return provider.id()
	}
	snapshot, _ := instanceSnapshot(instance)
	return snapshot.ID
}

func instanceQualifiedName(instance Instance) string {
	if instance == nil {
		return ""
	}
	if provider, ok := instance.(interface{ qualifiedName() string }); ok {
		return provider.qualifiedName()
	}
	snapshot, _ := instanceSnapshot(instance)
	return snapshot.QualifiedName
}

func instanceSnapshot(instance Instance) (Snapshot, bool) {
	if isNilValue(instance) {
		return Snapshot{}, false
	}
	snapshotter, ok := instance.(interface {
		takeSnapshot() Snapshot
	})
	if !ok {
		return Snapshot{}, false
	}
	return snapshotter.takeSnapshot(), true
}

func (sm *hsm[T]) Clock() Clock {
	if sm == nil {
		return DefaultClock
	}
	return sm.clock
}

// activeState returns the currently active state element, or nil when the
// machine has no observable active state because it was never started or is
// stopped. Liveness follows the shared instance registry: start registers the
// machine and stop deregisters it, while an expired context deadline alone
// does not mean the machine stopped.
func (sm *hsm[T]) activeState() *state {
	if sm == nil || sm.context == nil {
		return nil
	}
	instances, ok := sm.context.Value(Keys.Instances).(*sync.Map)
	if !ok {
		return nil
	}
	if _, live := instances.Load(sm.behavior.id); !live {
		return nil
	}
	active, _ := sm.state.Load().(*state)
	return active
}

func (sm *hsm[T]) State() string {
	state := sm.activeState()
	if state == nil {
		return ""
	}
	return state.QualifiedName()
}

func (sm *hsm[T]) start(ctx context.Context, event *Event) {
	if isStarted(sm) {
		panic(ErrAlreadyStarted)
	}
	sm.processing.wLock()
	sm.resetAttributes()
	instances, ok := ctx.Value(Keys.Instances).(*sync.Map)
	if !ok {
		instances = &sync.Map{}
	}
	sm.context, sm.cancel = context.WithCancel(context.WithValue(context.WithValue(context.WithValue(ctx, Keys.Instances, instances), Keys.Owner, ctx.Value(Keys.HSM)), Keys.HSM, sm))
	instances.Store(sm.behavior.id, sm)
	if !sm.execute(sm.activate(sm.context, sm), &sm.behavior, event) {
		sm.process(sm.context, "")
	}
}

func (sm *hsm[T]) restart(ctx context.Context, maybeData ...any) Completion {
	if !isStarted(sm) {
		return completedCompletion()
	}
	stopCtx, startCtx := restartContexts(ctx, sm)
	var data any
	if len(maybeData) > 0 {
		data = maybeData[0]
	}
	select {
	case err := <-sm.stop(stopCtx):
		if err != nil {
			return failedCompletion(err)
		}
	case <-stopCtx.Done():
		return completedCompletion()
	}
	if stopCtx.Err() != nil || isStarted(sm) {
		return completedCompletion()
	}
	initialEvent := InitialEvent.WithData(data)
	sm.start(startCtx, &initialEvent)
	return completionAfter(sm.processing.wait())
}

func restartContexts(ctx context.Context, instance Instance) (context.Context, context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	current, ok := ctx.Value(Keys.HSM).(Instance)
	if !ok || current != instance {
		return ctx, ctx
	}
	stopCtx := context.WithoutCancel(ctx)
	owner := ctx.Value(Keys.Owner)
	startCtx := context.WithValue(stopCtx, Keys.HSM, owner)
	return stopCtx, startCtx
}

func (sm *hsm[T]) wait() <-chan struct{} {
	return sm.processing.wait()
}

func (sm *hsm[T]) stop(ctx context.Context) Completion {
	if sm == nil {
		return completedCompletion()
	}
	signal := make(chan error)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("panic in stop", "error", r)
			}
			close(signal)
		}()
		sm.processing.wLock()
		hasQueuedEvents := false
		defer func() {
			sm.processing.wUnlock()
			if hasQueuedEvents && sm.processing.tryLock() {
				processCtx := context.WithValue(sm.processingContext(context.WithoutCancel(ctx)), processingContextKey, sm)
				go sm.processAfterStop(processCtx)
			}
		}()

		var ok bool
		state := sm.state.Load().(Element)
		for state != nil {
			select {
			case <-ctx.Done():
				return
			default:
				sm.exit(ctx, state, &FinalEvent)
				if ch, ok := sm.after.exited.LoadAndDelete(state.QualifiedName()); ok {
					close(ch.(chan struct{}))
				}
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

		queueLen, _ := sm.queue.len(sm.context)
		hasQueuedEvents = queueLen > 0
	}()
	return signal
}

func (sm *hsm[T]) Context() context.Context {
	if sm == nil {
		return nil
	}
	return sm.context
}

func (sm *hsm[T]) processingContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sm == nil {
		return ctx
	}
	if current, ok := ctx.Value(Keys.HSM).(Instance); ok && current == sm {
		return ctx
	}
	if instances, ok := sm.context.Value(Keys.Instances).(*sync.Map); ok && instances != nil {
		ctx = context.WithValue(ctx, Keys.Instances, instances)
	}
	ctx = context.WithValue(ctx, Keys.Owner, sm.context.Value(Keys.Owner))
	return context.WithValue(ctx, Keys.HSM, sm)
}

func (sm *hsm[T]) processContext(ctx context.Context) context.Context {
	if sm == nil {
		if ctx == nil {
			return context.Background()
		}
		return context.WithoutCancel(ctx)
	}
	if ctx == nil {
		return sm.context
	}
	if current, ok := ctx.Value(Keys.HSM).(Instance); ok && current == sm {
		return context.WithoutCancel(ctx)
	}
	if isRootContext(ctx) {
		return sm.context
	}
	return sm.processingContext(context.WithoutCancel(ctx))
}

func isRootContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	typ := reflect.TypeOf(ctx)
	if typ == nil || !typ.Comparable() {
		return false
	}
	return ctx == context.Background() || ctx == context.TODO()
}

func (sm *hsm[T]) get(name string) (any, bool) {
	if sm == nil {
		return nil, false
	}
	qualifiedName := qualifyModelName(sm.model.qualifiedName, name)
	value, ok := sm.attributes.Load(qualifiedName)
	if !ok {
		return nil, false
	}
	return cloneMetadataValue(value), true
}

func (sm *hsm[T]) set(ctx context.Context, name string, value any) Completion {
	if !isStarted(sm) {
		return failedCompletion(ErrInvalidState)
	}
	return sm.setAttribute(ctx, name, value, true)
}

func (sm *hsm[T]) setAttribute(ctx context.Context, name string, value any, emit bool) Completion {
	if sm == nil {
		return completedCompletion()
	}
	if ctx == nil {
		ctx = sm.context
	}
	qualifiedName := qualifyModelName(sm.model.qualifiedName, name)
	attr, known := sm.model.attributes[qualifiedName]
	if !known {
		return failedCompletion(fmt.Errorf("%w: %s", ErrUnknownAttribute, qualifiedName))
	}
	old, exists := sm.attributes.Load(qualifiedName)
	expectedType := attr.valueType()
	if exists {
		expectedType = reflect.TypeOf(old)
	}
	if !valueAssignableToType(value, expectedType) {
		return failedCompletion(fmt.Errorf("%w: %s", ErrInvalidAttributeType, qualifiedName))
	}
	sm.attributes.Store(qualifiedName, value)
	if !emit {
		return completedCompletion()
	}
	if exists && reflect.DeepEqual(old, value) {
		return completedCompletion()
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

func (sm *hsm[T]) call(ctx context.Context, name string, args ...any) (any, error) {
	if sm == nil {
		return nil, ErrNilHSM
	}
	if !isStarted(sm) {
		return nil, ErrInvalidState
	}
	if ctx == nil {
		ctx = sm.context
	}
	op, err := sm.lookupOperation(name)
	if err != nil {
		return nil, err
	}
	event := Event{
		Kind:   CallEventKind,
		Name:   op.name,
		Source: op.name,
		Data: CallData{
			Name: op.name,
			Args: args,
		},
	}
	result, err := sm.invokeOperation(op, ctx, args...)
	if err != nil {
		return result, err
	}
	sm.push(ctx, event)
	done := sm.scheduleProcess(ctx, "")
	if current, ok := ctx.Value(processingContextKey).(Instance); ok && current == sm {
		return result, nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	return result, nil
}

func (sm *hsm[T]) invokeOperationReference(ctx context.Context, name string, args ...any) (any, error) {
	if sm == nil {
		return nil, ErrNilHSM
	}
	if ctx == nil {
		ctx = sm.context
	}
	op, err := sm.lookupOperation(name)
	if err != nil {
		return nil, err
	}
	return sm.invokeOperation(op, ctx, args...)
}

func (sm *hsm[T]) lookupOperation(name string) (*operationDef, error) {
	if sm == nil {
		return nil, ErrNilHSM
	}
	if name == "" {
		return nil, ErrInvalidOperation
	}
	qualifiedName := qualifyModelName(sm.model.qualifiedName, name)
	op := sm.model.operations[qualifiedName]
	if op == nil {
		return nil, ErrMissingOperation
	}
	return op, nil
}

func (sm *hsm[T]) invokeOperation(op *operationDef, ctx context.Context, args ...any) (result any, err error) {
	if op == nil {
		return nil, ErrMissingOperation
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("operation %s panic: %v", op.name, recovered)
			result = nil
		}
	}()
	key := operationInvokerKey(op.name, args)
	if invoker, ok := sm.operations.Load(key); ok {
		return invoker.(operationInvoker)(ctx, args...)
	}
	invoker, err := sm.compileOperationInvoker(op, ctx, args...)
	if err != nil {
		return nil, err
	}
	actual, _ := sm.operations.LoadOrStore(key, invoker)
	return actual.(operationInvoker)(ctx, args...)
}

func (sm *hsm[T]) compileOperationInvoker(op *operationDef, ctx context.Context, args ...any) (operationInvoker, error) {
	fnType := op.fnType
	fnValue := op.fnValue
	if !fnValue.IsValid() || fnType == nil || fnType.Kind() != reflect.Func {
		fnValue, fnType = sm.operationMethod(op.name)
		if !fnValue.IsValid() || fnType == nil || fnType.Kind() != reflect.Func {
			return nil, ErrInvalidOperation
		}
	}
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
	for _, c := range candidates {
		spec := operationCallSpec{
			fnValue:     fnValue,
			fnType:      fnType,
			useCtx:      c.useCtx,
			useInstance: c.useInstance,
		}
		if _, ok := sm.operationCallArgs(spec, ctx, args); !ok {
			continue
		}
		return func(ctx context.Context, args ...any) (any, error) {
			callArgs, ok := sm.operationCallArgs(spec, ctx, args)
			if !ok {
				return nil, ErrInvalidOperation
			}
			return operationCallResult(spec.fnValue.Call(callArgs))
		}, nil
	}
	return nil, ErrInvalidOperation
}

func operationInvokerKey(name string, args []any) operationInvokerCacheKey {
	key := operationInvokerCacheKey{name: name, argCount: len(args)}
	if len(args) <= len(key.argTypes) {
		for i, arg := range args {
			if arg != nil {
				key.argTypes[i] = reflect.TypeOf(arg)
			}
		}
		return key
	}
	var builder strings.Builder
	for _, arg := range args[len(key.argTypes):] {
		if arg == nil {
			builder.WriteString("<nil>")
		} else {
			builder.WriteString(reflect.TypeOf(arg).String())
		}
		builder.WriteByte(';')
	}
	for i := range key.argTypes {
		if args[i] != nil {
			key.argTypes[i] = reflect.TypeOf(args[i])
		}
	}
	key.overflow = builder.String()
	return key
}

func (sm *hsm[T]) operationCallArgs(spec operationCallSpec, ctx context.Context, args []any) ([]reflect.Value, bool) {
	callArgs := make([]reflect.Value, 0, spec.fnType.NumIn())
	argIndex := 0
	if spec.useCtx {
		if argIndex >= spec.fnType.NumIn() {
			return nil, false
		}
		ctxValue := reflect.ValueOf(ctx)
		var ok bool
		callArgs, ok = appendAssignableValue(callArgs, ctxValue, spec.fnType.In(argIndex))
		if !ok {
			return nil, false
		}
		argIndex++
	}
	if spec.useInstance {
		if argIndex >= spec.fnType.NumIn() {
			return nil, false
		}
		var ok bool
		callArgs, ok = appendInstanceValue(callArgs, reflect.ValueOf(sm.instance), spec.fnType.In(argIndex))
		if !ok {
			return nil, false
		}
		argIndex++
	}
	remainingParams := spec.fnType.NumIn() - argIndex
	if !spec.fnType.IsVariadic() {
		if remainingParams != len(args) {
			return nil, false
		}
	} else if len(args) < remainingParams-1 {
		return nil, false
	}
	for i := 0; i < len(args); i++ {
		paramIndex := argIndex + i
		var paramType reflect.Type
		if spec.fnType.IsVariadic() && paramIndex >= spec.fnType.NumIn()-1 {
			paramType = spec.fnType.In(spec.fnType.NumIn() - 1).Elem()
		} else if paramIndex < spec.fnType.NumIn() {
			paramType = spec.fnType.In(paramIndex)
		} else {
			return nil, false
		}
		var ok bool
		callArgs, ok = appendAssignableValue(callArgs, reflect.ValueOf(args[i]), paramType)
		if !ok {
			return nil, false
		}
	}
	return callArgs, true
}

func appendInstanceValue(values []reflect.Value, instanceValue reflect.Value, paramType reflect.Type) ([]reflect.Value, bool) {
	if values, ok := appendAssignableValue(values, instanceValue, paramType); ok {
		return values, true
	}
	if instanceValue.Kind() == reflect.Pointer && instanceValue.Elem().IsValid() {
		return appendAssignableValue(values, instanceValue.Elem(), paramType)
	}
	return nil, false
}

func appendAssignableValue(values []reflect.Value, value reflect.Value, paramType reflect.Type) ([]reflect.Value, bool) {
	if !value.IsValid() {
		if nilAssignableTo(paramType.Kind()) {
			return append(values, reflect.Zero(paramType)), true
		}
		return nil, false
	}
	if value.Type().AssignableTo(paramType) {
		return append(values, value), true
	}
	if value.Type().ConvertibleTo(paramType) {
		return append(values, value.Convert(paramType)), true
	}
	return nil, false
}

func nilAssignableTo(kind reflect.Kind) bool {
	return kind == reflect.Interface ||
		kind == reflect.Pointer ||
		kind == reflect.Slice ||
		kind == reflect.Map ||
		kind == reflect.Func ||
		kind == reflect.Chan
}

func operationCallResult(results []reflect.Value) (any, error) {
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

func (sm *hsm[T]) operationMethod(name string) (reflect.Value, reflect.Type) {
	if sm == nil {
		return reflect.Value{}, nil
	}
	instance := reflect.ValueOf(sm.instance)
	if !instance.IsValid() || ((instance.Kind() == reflect.Chan || instance.Kind() == reflect.Func || instance.Kind() == reflect.Interface || instance.Kind() == reflect.Map || instance.Kind() == reflect.Pointer || instance.Kind() == reflect.Slice) && instance.IsNil()) {
		return reflect.Value{}, nil
	}
	simpleName := path.Base(name)
	candidates := []string{simpleName}
	if simpleName != "" {
		candidates = append(candidates, strings.ToUpper(simpleName[:1])+simpleName[1:])
	}
	for _, candidate := range candidates {
		method := instance.MethodByName(candidate)
		if method.IsValid() {
			return method, method.Type()
		}
	}
	return reflect.Value{}, nil
}

func (sm *hsm[T]) channels() *after {
	if sm == nil {
		return nil
	}
	return &sm.after
}

func (sm *hsm[T]) modelBehavior(name string) *behavior[T] {
	if sm == nil || name == "" {
		return nil
	}
	return sm.behaviors[name]
}

func (sm *hsm[T]) modelConstraint(name string) *constraint[T] {
	if sm == nil || name == "" {
		return nil
	}
	return sm.constraints[name]
}

func (sm *hsm[T]) bindModelCaches() {
	if sm == nil || sm.model == nil || sm.model.Model == nil {
		return
	}
	for name := range sm.model.members {
		if behavior := getBehavior[T](sm.model.Model, name); behavior != nil {
			sm.behaviors[name] = behavior
		}
		if constraint := getConstraint[T](sm.model.Model, name); constraint != nil {
			sm.constraints[name] = constraint
		}
	}
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
		if behavior := sm.modelBehavior(qualifiedName); behavior != nil {
			sm.execute(ctx, behavior, event)
		}
	}
}

func (sm *hsm[T]) enter(ctx context.Context, element Element, event *Event, defaultEntry bool) Element {
	if sm == nil {
		return nil
	}
	switch element.Kind() {
	case StateKind, SubmachineStateKind:
		state := element.(*state)
		for _, entry := range state.entry {
			if entry := sm.modelBehavior(entry); entry != nil {
				if !sm.execute(ctx, entry, event) {
					return nil
				}
			}
		}
		if len(state.activities) > 0 {
			sm.executeAll(ctx, state.activities, event)
		}
		if !defaultEntry || state.initial == "" {
			return state
		}
		if initial := get[*vertex](sm.model.Model, state.initial); initial != nil {
			if len(initial.transitions) > 0 {
				if transition := get[*transition](sm.model.Model, initial.transitions[0]); transition != nil {
					if next := sm.transition(ctx, state, transition, event); next != nil {
						return next
					}
					return nil
				}
			}
		}
		return state
	case ChoiceKind:
		choiceVertex := element.(*vertex)
		transition, ok := sm.firstEnabledTransition(ctx, choiceVertex.transitions, event, nil)
		if !ok {
			return nil
		}
		if transition != nil {
			return sm.transition(ctx, element, transition, event)
		}
	case EntryPointKind:
		entryPoint := element.(*vertex)
		transition, ok := sm.firstEnabledTransition(ctx, entryPoint.transitions, event, nil)
		if !ok {
			return nil
		}
		if transition != nil {
			return sm.transition(ctx, element, transition, event)
		}
		return element
	case ExitPointKind:
		exitPoint := element.(*vertex)
		for _, qualifiedName := range exitPoint.transitions {
			transition := get[*transition](sm.model.Model, qualifiedName)
			if transition == nil || transition.Owner() != exitPoint.QualifiedName() || transition.target != exitPoint.Owner() {
				continue
			}
			matches, ok := sm.transitionEnabled(ctx, transition, event)
			if !ok {
				return nil
			}
			if !matches {
				continue
			}
			if sm.transition(ctx, element, transition, event) == nil {
				return nil
			}
		}
		transition, ok := sm.firstEnabledTransition(ctx, exitPoint.transitions, event, func(transition *transition) bool {
			return transition.Owner() != exitPoint.QualifiedName() || transition.target != exitPoint.Owner()
		})
		if !ok {
			return nil
		}
		if transition != nil {
			return sm.transition(ctx, element, transition, event)
		}
		panic(fmt.Errorf("unhandled exit point %q", exitPoint.Name()))
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
			if parentState := get[*state](sm.model.Model, parent); parentState != nil && parentState.initial != "" {
				if initialVertex := get[*vertex](sm.model.Model, parentState.initial); initialVertex != nil {
					if len(initialVertex.transitions) > 0 {
						if transition := get[*transition](sm.model.Model, initialVertex.transitions[0]); transition != nil {
							return sm.transition(ctx, parentState, transition, event)
						}
					}
				}
			}
			return element
		}
		enterPath := sm.model.historyPaths[parent][resolved]
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
		completionEvent := FinalEvent
		completionEvent.Source = element.QualifiedName()
		sm.push(ctx, completionEvent)
		if element.Owner() == sm.model.qualifiedName && len(sm.model.transitionMap[element.QualifiedName()][FinalEvent.Name]) == 0 {
			sm.cancel()
		}
		return element
	}
	return nil
}

func (sm *hsm[T]) exit(ctx context.Context, element Element, event *Event) bool {
	if sm == nil || element == nil {
		return true
	}
	if state, ok := element.(*state); ok {
		// if len(state.activities) > 0 {
		// 	sm.terminateAll(ctx, state.activities)
		// }
		for _, activity := range state.activities {
			if activity := sm.modelBehavior(activity); activity != nil {
				sm.terminate(ctx, activity)
			}
		}
		for _, exit := range state.exit {
			if exit := sm.modelBehavior(exit); exit != nil {
				if !sm.execute(ctx, exit, event) {
					return false
				}
			}
		}
	}
	return true
}

func (sm *hsm[T]) recordHistory(stateName, skipOwner string) {
	if sm == nil || stateName == "" {
		return
	}
	for historyName, target := range sm.model.historyTargets[historyTargetKey{stateName: stateName, skipOwner: skipOwner}] {
		history := sm.model.members[historyName]
		parent := path.Dir(historyName)
		if history == nil || parent == "." || parent == "/" {
			continue
		}
		if kind.Is(history.Kind(), ShallowHistoryKind) {
			sm.historyShallow[parent] = target
		} else if kind.Is(history.Kind(), DeepHistoryKind) {
			sm.historyDeep[parent] = target
		}
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
	transition, ok := sm.firstEnabledTransition(ctx, historyVertex.transitions, event, nil)
	if !ok {
		return nil
	}
	if transition != nil {
		return sm.transition(ctx, historyVertex, transition, event)
	}
	return nil
}

func completeExecution[T Instance](sm *hsm[T], element Element) {
	if sm == nil || element == nil {
		return
	}
	if ch, ok := sm.after.executed.LoadAndDelete(element.QualifiedName()); ok {
		close(ch.(chan struct{}))
	}
	if owner := element.Owner(); owner != "" {
		if ownerElement, ok := sm.model.members[owner]; ok && kind.Is(ownerElement.Kind(), StateKind) {
			if ch, ok := sm.after.executed.LoadAndDelete(owner); ok {
				close(ch.(chan struct{}))
			}
		}
	}
}

func cleanupConcurrent[T Instance](ctx context.Context, sm *hsm[T], element Element, recovered any) {
	if recovered != nil {
		slog.Error("hsm: panic in concurrent behavior", "error", recovered, "stack", string(debug.Stack()))
		if active, ok := sm.active[element.QualifiedName()]; ok {
			select {
			case active.channel <- struct{}{}:
			default:
			}
		}
		go sm.dispatch(ctx, ErrorEvent.WithData(fmt.Errorf("panic in concurrent behavior %s: %s", element.QualifiedName(), recovered)))
	}
	completeExecution(sm, element)
}

func cleanupSynchronous[T Instance](ctx context.Context, sm *hsm[T], element Element, recovered any) bool {
	if recovered == nil {
		completeExecution(sm, element)
		return true
	}
	slog.Error("hsm: panic in behavior", "error", recovered, "stack", string(debug.Stack()))
	sm.push(ctx, ErrorEvent.WithData(fmt.Errorf("panic in behavior %s: %s", element.QualifiedName(), recovered)))
	completeExecution(sm, element)
	return false
}

func (sm *hsm[T]) execute(ctx context.Context, element *behavior[T], event *Event) (ok bool) {
	if sm == nil || element == nil {
		return true
	}
	switch element.Kind() {
	case ConcurrentKind:
		ctx := sm.activate(ctx, element)
		go func(ctx *active, event Event) {
			defer func() {
				cleanupConcurrent(ctx, sm, element, recover())
			}()
			element.operation(ctx, sm.instance, event)
			ctx.channel <- struct{}{}
		}(ctx, *event)
		return true
	default:
		ok = true
		defer func() {
			if recovered := recover(); recovered != nil {
				ok = cleanupSynchronous(ctx, sm, element, recovered)
				return
			}
			ok = cleanupSynchronous(ctx, sm, element, nil)
		}()
		element.operation(ctx, sm.instance, *event)
		return ok
	}
}

func (sm *hsm[T]) evaluate(ctx context.Context, guard *constraint[T], event *Event) (matches bool, ok bool) {
	if sm == nil || guard == nil || guard.expression == nil {
		return true, true
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			matches = false
			ok = false
			slog.Error("hsm: panic in guard", "error", recovered, "stack", string(debug.Stack()))
			sm.push(ctx, ErrorEvent.WithData(fmt.Errorf("panic in guard %s: %s", guard.QualifiedName(), recovered)))
		}
	}()
	return guard.expression(
		ctx,
		sm.instance,
		*event,
	), true
}

func (sm *hsm[T]) transitionEnabled(ctx context.Context, transition *transition, event *Event) (matches bool, ok bool) {
	if guard := sm.modelConstraint(transition.Guard()); guard != nil {
		return sm.evaluate(ctx, guard, event)
	}
	return true, true
}

func (sm *hsm[T]) firstEnabledTransition(ctx context.Context, transitions []string, event *Event, accept func(*transition) bool) (*transition, bool) {
	for _, qualifiedName := range transitions {
		transition := get[*transition](sm.model.Model, qualifiedName)
		if transition == nil || accept != nil && !accept(transition) {
			continue
		}
		matches, ok := sm.transitionEnabled(ctx, transition, event)
		if !ok {
			return nil, false
		}
		if !matches {
			continue
		}
		return transition, true
	}
	return nil, true
}

func (sm *hsm[T]) transition(ctx context.Context, current Element, transition *transition, event *Event) Element {
	if sm == nil {
		return nil
	}
	path, ok := sm.model.transitionPaths[transition][current.QualifiedName()]
	if !ok {
		return nil
	}
	skipHistoryOwner := ""
	if transition.target != "" {
		if target := sm.model.members[transition.target]; target != nil && (kind.Is(target.Kind(), ShallowHistoryKind) || kind.Is(target.Kind(), DeepHistoryKind)) {
			skipHistoryOwner = target.Owner()
		}
	}
	if len(path.exit) > 0 {
		sm.recordHistory(current.QualifiedName(), skipHistoryOwner)
	}
	for _, exiting := range path.exit {
		current, ok = sm.model.members[exiting]
		if !ok {
			return nil
		}
		if !sm.exit(ctx, current, event) {
			return nil
		}
		if ch, ok := sm.after.exited.LoadAndDelete(exiting); ok {
			close(ch.(chan struct{}))
		}
	}
	for _, effect := range transition.effect {
		if effect := sm.modelBehavior(effect); effect != nil {
			if !sm.execute(ctx, effect, event) {
				return nil
			}
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
		if current == nil {
			return nil
		}
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
	case <-sm.clock.After(sm.timeouts.activity):
		sm.push(ctx, ErrorEvent.WithData(fmt.Errorf("terminate timeout: %s", element.QualifiedName())))
	}

}

func (sm *hsm[T]) process(ctx context.Context, currentEventID string, completions ...chan error) {
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("hsm: panic while processing event in state machine: %v\n\n%s", r, string(debug.Stack()))
			slog.Error("hsm: panic while processing event in state machine", "error", err)
			go sm.dispatch(ctx, ErrorEvent.WithData(err))
		}
		if len(completions) > 0 && completions[0] != nil {
			close(completions[0])
		}
		sm.processing.wUnlock()
	}()
	if sm == nil {
		return
	}
	type deferredEvent struct {
		owner string
		event Event
	}
	var deferred []deferredEvent
	event, ok := sm.pop(ctx)
	for ok {
		eventID := event.ID
		if isInternalEventID(eventID) {
			event.ID = ""
		}
		if currentEventID != "" && eventID != currentEventID {
			currentState := sm.state.Load().(Element)
			if currentState != nil {
				if deferredSet, ok := sm.model.deferredMap[currentState.QualifiedName()]; ok {
					if deferOwner := deferredSet[event.Name]; deferOwner != "" {
						deferred = append(deferred, deferredEvent{owner: deferOwner, event: event})
						event, ok = sm.pop(ctx)
						continue
					}
				}
			}
		}
		transitionTaken, deferOwner, transitionSource := sm.processEvent(ctx, &event)
		if deferOwner != "" {
			deferred = append(deferred, deferredEvent{owner: deferOwner, event: event})
			event, ok = sm.pop(ctx)
			continue
		}
		if transitionTaken && len(deferred) > 0 {
			activeState := sm.state.Load().(Element).QualifiedName()
			for _, deferredEvent := range deferred {
				discard := false
				current := path.Dir(deferredEvent.owner)
				for current != "" && current != "." && current != "/" {
					currentState := sm.model.members[current]
					if currentState != nil && current != sm.model.qualifiedName && kind.Is(currentState.Kind(), SubmachineStateKind) {
						discard = activeState != current &&
							!IsAncestor(current, activeState) &&
							!(transitionSource != "" && IsAncestor(current, transitionSource))
						break
					}
					if current == sm.model.qualifiedName {
						break
					}
					current = path.Dir(current)
				}
				if !discard {
					sm.push(ctx, deferredEvent.event)
				}
			}
			deferred = nil
		}
		event, ok = sm.pop(ctx)
	}
	for _, deferredEvent := range deferred {
		sm.push(ctx, deferredEvent.event)
	}
}

func (sm *hsm[T]) processAfterStop(ctx context.Context) {
	if sm == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("hsm: panic while processing post-stop events: %v\n\n%s", r, string(debug.Stack()))
			slog.Error("hsm: panic while processing post-stop events", "error", err)
		}
		sm.processing.wUnlock()
	}()
	event, ok := sm.pop(ctx)
	for ok {
		if kind.Is(event.Kind, ErrorEventKind) {
			sm.processEvent(ctx, &event)
		}
		event, ok = sm.pop(ctx)
	}
}

func (sm *hsm[T]) push(ctx context.Context, event Event) {
	if sm == nil {
		return
	}
	if err := sm.queue.push(ctx, event); err != nil && !kind.Is(event.Kind, ErrorEventKind) {
		_ = sm.queue.push(ctx, ErrorEvent.WithData(err))
	}
}

func (sm *hsm[T]) pop(ctx context.Context) (Event, bool) {
	if sm == nil {
		return empty, false
	}
	for {
		event, ok, err := sm.queue.pop(ctx)
		if err == nil {
			return event, ok
		}
		_ = sm.queue.push(ctx, ErrorEvent.WithData(err))
	}
}

func (sm *hsm[T]) processEvent(ctx context.Context, event *Event) (transitionTaken bool, deferOwner string, transitionSource string) {
	if sm == nil || event == nil {
		return false, "", ""
	}
	if event.Kind == 0 {
		event.Kind = EventKind
	}
	currentState := sm.state.Load().(Element)
	currentQualifiedName := currentState.QualifiedName()
	if deferredSet, ok := sm.model.deferredMap[currentQualifiedName]; ok {
		deferOwner = deferredSet[event.Name]
	}

	// Direct O(1) lookup for transitions - no hierarchy walking needed.
	for pass := 0; pass < 2 && !transitionTaken; pass++ {
		eventName := event.Name
		if pass == 1 {
			if event.Name == AnyEvent.Name {
				break
			}
			eventName = AnyEvent.Name
		}
		transitions := sm.model.transitionMap[currentQualifiedName][eventName]
		for _, transition := range transitions {
			if deferOwner != "" && !transitionHandlesAtOrBelow(transition, deferOwner, sm.model.qualifiedName) {
				continue
			}
			matches, ok := sm.transitionEnabled(ctx, transition, event)
			if !ok {
				return false, deferOwner, transitionSource
			}
			if !matches {
				continue
			}
			state := sm.transition(ctx, currentState, transition, event)
			if state != nil {
				sm.state.Store(state)
				transitionTaken = true
				transitionSource = transition.source
			}
			break
		}
	}

	if !transitionTaken && deferOwner != "" {
		return false, deferOwner, ""
	}

	if ch, ok := sm.after.processed.LoadAndDelete(event.Name); ok {
		close(ch.(chan struct{}))
	}
	return transitionTaken, "", transitionSource
}

func (sm *hsm[T]) takeSnapshot() Snapshot {
	if sm == nil {
		return Snapshot{}
	}
	if !isStarted(sm) {
		return Snapshot{
			ID:            sm.behavior.id,
			QualifiedName: sm.qualifiedName(),
			State:         sm.model.QualifiedName(),
		}
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
		attributes[name] = cloneMetadataValue(value)
		return true
	})

	var events []EventSnapshot
	var transitions []TransitionSnapshot

	availableByTransition := map[*transition]map[string]struct{}{}
	for eventName, indexedTransitions := range sm.model.transitionMap[state.QualifiedName()] {
		for _, transition := range indexedTransitions {
			if transition == nil {
				continue
			}
			if availableByTransition[transition] == nil {
				availableByTransition[transition] = map[string]struct{}{}
			}
			availableByTransition[transition][eventName] = struct{}{}
		}
	}
	seenTransitions := map[*transition]struct{}{}
	appendTransitionSnapshot := func(transition *transition) {
		if transition == nil {
			return
		}
		availableEvents := availableByTransition[transition]
		if len(availableEvents) == 0 {
			return
		}
		eventNames := make([]string, 0, len(availableEvents))
		for _, eventName := range transition.events {
			if _, ok := availableEvents[eventName]; ok {
				eventNames = append(eventNames, eventName)
			}
		}
		if len(eventNames) == 0 {
			return
		}
		seenTransitions[transition] = struct{}{}
		hasGuard := transition.guard != ""
		transitions = append(transitions, TransitionSnapshot{
			Name:   transition.QualifiedName(),
			Kind:   transition.Kind(),
			Source: transition.Source(),
			Target: transition.Target(),
			Events: eventNames,
			Guard:  hasGuard,
		})
		for _, eventName := range eventNames {
			event, exists := sm.model.events[eventName]
			if !exists || !kind.Is(event.Kind, EventKind) {
				continue
			}
			events = append(events, EventSnapshot{
				Name:   eventName,
				Kind:   event.Kind,
				Target: transition.Target(),
				Guard:  hasGuard,
				Schema: cloneMetadataValue(event.Schema),
			})
		}
	}
	for current := state; current != nil; {
		if vertex, ok := current.(interface{ Transitions() []string }); ok {
			for _, transitionName := range vertex.Transitions() {
				appendTransitionSnapshot(get[*transition](sm.model.Model, transitionName))
			}
		}
		owner := current.Owner()
		next, ok := sm.model.members[owner]
		if !ok || next == current {
			break
		}
		current = next
	}
	leftovers := make([]*transition, 0)
	for transition := range availableByTransition {
		if _, seen := seenTransitions[transition]; !seen {
			leftovers = append(leftovers, transition)
		}
	}
	sort.Slice(leftovers, func(i, j int) bool {
		return leftovers[i].QualifiedName() < leftovers[j].QualifiedName()
	})
	for _, transition := range leftovers {
		appendTransitionSnapshot(transition)
	}

	queueLen, err := sm.queue.len(sm.context)
	if err != nil {
		queueLen = 0
	}
	return Snapshot{
		ID:            sm.behavior.id,
		QualifiedName: sm.behavior.qualifiedName,
		State:         state.QualifiedName(),
		Attributes:    attributes,
		QueueLen:      queueLen,
		Events:        events,
		Transitions:   transitions,
	}
}

func (sm *hsm[T]) dispatch(ctx context.Context, event Event) Completion {
	if sm == nil {
		return failedCompletion(ErrNilHSM)
	}
	if !isStarted(sm) {
		return failedCompletion(ErrInvalidState)
	}
	state := sm.state.Load().(Element)
	if state == nil {
		return failedCompletion(ErrInvalidState)
	}
	if event.Kind == 0 {
		event.Kind = EventKind
	}
	currentEventID := event.ID
	if currentEventID == "" {
		if deferredSet := sm.model.deferredMap[state.QualifiedName()]; len(deferredSet) > 0 {
			event.ID = nextInternalEventID()
			currentEventID = event.ID
		}
	}
	sm.push(ctx, event)
	done := sm.scheduleProcess(ctx, currentEventID)
	if ch, ok := sm.after.dispatched.LoadAndDelete(event.Name); ok {
		close(ch.(chan struct{}))
	}
	return done
}

func (sm *hsm[T]) scheduleProcess(ctx context.Context, currentEventID string) Completion {
	if sm.processing.tryLock() {
		done := make(chan error)
		processCtx := context.WithValue(sm.processContext(ctx), processingContextKey, sm)
		go sm.process(processCtx, currentEventID, done)
		return done
	}
	signal := make(chan error)
	sm.drain.mutex.Lock()
	sm.drain.waiters = append(sm.drain.waiters, signal)
	if sm.drain.ctx == nil {
		sm.drain.ctx = ctx
	}
	if sm.drain.eventID == "" {
		sm.drain.eventID = currentEventID
	}
	if !sm.drain.scheduled {
		sm.drain.scheduled = true
		go sm.runScheduledDrain(sm.processing.wait())
	}
	sm.drain.mutex.Unlock()
	return signal
}

func (sm *hsm[T]) runScheduledDrain(done <-chan struct{}) {
	for {
		<-done
		for {
			if sm.processing.tryLock() {
				ctx, eventID, waiters := sm.takeDrainBatch()
				drained := sm.processing.wait()
				processCtx := context.WithValue(sm.processContext(ctx), processingContextKey, sm)
				go sm.process(processCtx, eventID)
				<-drained
				closeDrainWaiters(waiters)
				sm.drain.mutex.Lock()
				if len(sm.drain.waiters) == 0 {
					sm.drain.scheduled = false
					sm.drain.mutex.Unlock()
					return
				}
				done = sm.processing.wait()
				sm.drain.mutex.Unlock()
				break
			}
			next := sm.processing.wait()
			if next == done {
				runtime.Gosched()
			}
			done = next
		}
	}
}

func (sm *hsm[T]) takeDrainBatch() (context.Context, string, []chan error) {
	sm.drain.mutex.Lock()
	defer sm.drain.mutex.Unlock()
	ctx := sm.drain.ctx
	eventID := sm.drain.eventID
	waiters := sm.drain.waiters
	sm.drain.ctx = nil
	sm.drain.eventID = ""
	sm.drain.waiters = nil
	return ctx, eventID, waiters
}

func closeDrainWaiters(waiters []chan error) {
	for _, waiter := range waiters {
		close(waiter)
	}
}

// Dispatch sends an event to a specific state machine instance.
// It returns a completion channel that yields the runtime error, if any, when
// the event has been fully processed. A nil error means success.
//
// Example:
//
//	model := hsm.Define(...)
//	sm := hsm.Started(context.Background(), &MyHSM{}, &model)
//	done := hsm.Dispatch(context.Background(), sm, hsm.Event{Name: "start"})
//	if err := <-done; err != nil {
//		// Handle the dispatch failure.
//	}
func Dispatch[T context.Context](ctx T, hsm Instance, event Event) Completion {
	if hsm != nil {
		return hsm.dispatch(ctx, event)
	}
	// get the hsm from the context
	if hsm, ok := FromContext(ctx); ok {
		// dispatch the event to the hsm
		return hsm.dispatch(ctx, event)
	}
	return failedCompletion(ErrMissingHSM)
}

// Get reads an attribute value from the given state machine or from context.
func Get[T stringLike](ctx context.Context, hsm Instance, name T) (any, bool) {
	attributeName := string(name)
	if hsm != nil {
		return hsm.get(attributeName)
	}
	if hsm, ok := FromContext(ctx); ok {
		return hsm.get(attributeName)
	}
	return nil, false
}

// Set updates an attribute value and emits an OnSet change event.
// It returns a completion channel that yields the runtime error, if any, after
// the resulting processing completes. A nil error means success.
func Set[T stringLike](ctx context.Context, hsm Instance, name T, value any) Completion {
	attributeName := string(name)
	if hsm != nil {
		return hsm.set(ctx, attributeName, value)
	}
	if hsm, ok := FromContext(ctx); ok {
		return hsm.set(ctx, attributeName, value)
	}
	return failedCompletion(ErrMissingHSM)
}

// Call dispatches an OnCall event and invokes the named operation.
func Call[T stringLike](ctx context.Context, hsm Instance, name T, args ...any) (any, error) {
	operationName := string(name)
	if hsm != nil {
		return hsm.call(ctx, operationName, args...)
	}
	if hsm, ok := FromContext(ctx); ok {
		return hsm.call(ctx, operationName, args...)
	}
	return nil, ErrMissingHSM
}

// DispatchAll sends an event to all state machine instances in the current
// context. It returns a completion channel that yields the first runtime error,
// if any, after all selected instances have processed the event. A nil error
// means success.
//
// Example:
//
//	sm1 := hsm.Started(context.Background(), &MyHSM{}, &model)
//	sm2 := hsm.Started(sm1.Context(), &MyHSM{}, &model)
//	done := hsm.DispatchAll(sm2.Context(), hsm.Event{Name: "globalEvent"})
//	<-done // Wait for all instances to process the event
func DispatchAll(ctx context.Context, event Event) Completion {
	return DispatchTo[string](ctx, event)
}

// DispatchTo sends an event to the selected state machine instances in the
// current context. It returns a completion channel that yields the first
// runtime error, if any, after all matching instances have processed the event.
// A nil error means success.
func DispatchTo[T stringLike](ctx context.Context, event Event, maybeIds ...T) Completion {
	if ctx == nil {
		return completedCompletion()
	}
	instances, ok := ctx.Value(Keys.Instances).(*sync.Map)
	if !ok || instances == nil {
		return completedCompletion()
	}
	signal := make(chan error, 1)
	go func(signal chan error) {
		defer close(signal)
		signals := make(map[string]<-chan error)
		var firstErr error
		source := ""
		if current, ok := FromContext(ctx); ok {
			source = ID(current)
		}
		instances.Range(func(key, value any) bool {
			instance := value.(Instance)
			targetID := ID(instance)
			if len(maybeIds) == 0 || Match(targetID, maybeIds...) {
				targetedEvent := eventForTarget(event, source, targetID)
				signals[key.(string)] = instance.dispatch(ctx, targetedEvent)
			}
			return true
		})
		for len(signals) > 0 {
			for i, ch := range signals {
				select {
				case err := <-ch:
					if firstErr == nil {
						firstErr = err
					}
					delete(signals, i)
				case <-ctx.Done():
					return
				}
			}
		}
		if firstErr != nil {
			signal <- firstErr
		}
	}(signal)
	return signal
}

func eventForTarget(event Event, source, target string) Event {
	targeted := event
	if targeted.Source == "" {
		targeted.Source = source
	}
	if targeted.Target == "" {
		targeted.Target = target
	}
	return targeted
}

// AfterProcess returns a channel that closes when event processing completes.
// If an event is provided, the channel closes after that specific event is
// processed. If no event is provided, the channel closes after the next
// processing cycle completes. Use this helper for tests and deterministic
// observation only; production callers should wait on the completion channel
// returned by Dispatch, Set, Restart, Stop, DispatchAll, or DispatchTo.
func AfterProcess(ctx context.Context, hsm Instance, maybeEvent ...Event) <-chan struct{} {
	if len(maybeEvent) > 0 {
		ch, _ := hsm.channels().processed.LoadOrStore(maybeEvent[0].Name, make(chan struct{}))
		return ch.(chan struct{})
	} else {
		return hsm.wait()
	}
}

// AfterDispatch returns a channel that closes when the specified event is
// dispatched. Unlike AfterProcess, this signals when the event is added to
// the queue, not when processing completes. Use this helper for tests and
// deterministic observation only; it is not part of the supported production
// synchronization path.
func AfterDispatch(ctx context.Context, hsm Instance, event Event) <-chan struct{} {
	ch, _ := hsm.channels().dispatched.LoadOrStore(event.Name, make(chan struct{}))
	return ch.(chan struct{})
}

// AfterEntry returns a channel that closes when the specified state is
// entered. The state parameter should be the fully qualified state path
// (e.g., "/parent/child"). Use this helper for tests and deterministic
// observation only; it is not part of the supported production
// synchronization path.
func AfterEntry[T stringLike](ctx context.Context, hsm Instance, state T) <-chan struct{} {
	ch, _ := hsm.channels().entered.LoadOrStore(string(state), make(chan struct{}))
	return ch.(chan struct{})
}

// AfterExit returns a channel that closes when the specified state is exited.
// The state parameter should be the fully qualified state path
// (e.g., "/parent/child"). Use this helper for tests and deterministic
// observation only; it is not part of the supported production
// synchronization path.
func AfterExit[T stringLike](ctx context.Context, hsm Instance, state T) <-chan struct{} {
	ch, _ := hsm.channels().exited.LoadOrStore(string(state), make(chan struct{}))
	return ch.(chan struct{})
}

// AfterExecuted returns a channel that closes when the specified state's
// do-activity has completed execution. The state parameter should be the
// fully qualified state path. Use this helper for tests and deterministic
// observation only; it is not part of the supported production
// synchronization path.
func AfterExecuted[T stringLike](ctx context.Context, hsm Instance, state T) <-chan struct{} {
	ch, _ := hsm.channels().executed.LoadOrStore(string(state), make(chan struct{}))
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
	if ctx == nil {
		return nil, false
	}
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
	if ctx == nil {
		return nil, false
	}
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
// It returns a completion channel that yields the runtime error, if any, after
// shutdown processing finishes. A nil error means success. If no instance is
// available, Stop returns an already successful completion channel.
//
// Example:
//
//	sm := hsm.Started(context.Background(), &MyHSM{}, &model)
//	// ... use state machine ...
//	<-hsm.Stop(context.Background(), sm)
func Stop(ctx context.Context, hsm Instance) Completion {
	if hsm != nil {
		return hsm.stop(ctx)
	}
	if hsm, ok := FromContext(ctx); ok {
		return hsm.stop(ctx)
	}
	return completedCompletion()
}

// Restart stops a state machine and restarts it from the initial state.
// Optional data can be passed to reinitialize the state machine's data field.
// It returns a completion channel that yields the runtime error, if any, when
// the restart completes. A nil error means success.
func Restart(ctx context.Context, hsm Instance, maybeData ...any) Completion {
	if !isNilValue(hsm) {
		if !isStarted(hsm) {
			return failedCompletion(ErrInvalidState)
		}
		return hsm.restart(ctx, maybeData...)
	}
	if hsm, ok := FromContext(ctx); ok {
		if !isStarted(hsm) {
			return failedCompletion(ErrInvalidState)
		}
		return hsm.restart(ctx, maybeData...)
	}
	return failedCompletion(ErrMissingHSM)
}

// ID returns the unique identifier of a state machine instance.
// The ID is assigned when the state machine is created and remains
// constant throughout its lifecycle.
func ID(hsm Instance) string {
	if hsm == nil {
		return ""
	}
	return instanceID(hsm)
}

// QualifiedName returns the fully qualified name of a state machine instance.
// For nested state machines, this includes the parent path (e.g., "/parent/child").
// For top-level state machines, this is typically just the name with a leading slash.
func QualifiedName(hsm Instance) string {
	if hsm == nil {
		return ""
	}
	return instanceQualifiedName(hsm)
}

// Name returns the simple name of a state machine instance (without path prefix).
// This extracts the base name from the qualified name, e.g., "child" from "/parent/child".
func Name(hsm Instance) string {
	return path.Base(QualifiedName(hsm))
}

type snapshotTarget[S any] interface {
	takeSnapshot() S
}

// TakeSnapshot captures the current state of a machine or group.
// For a state machine it returns Snapshot. For a Group it returns []Snapshot
// in group order.
func TakeSnapshot[S any, T snapshotTarget[S]](ctx context.Context, hsm T) S {
	if !isNilValue(hsm) {
		return hsm.takeSnapshot()
	}
	var zero S
	if current, ok := FromContext(ctx); ok {
		if snapshotter, ok := any(current).(snapshotTarget[S]); ok {
			return snapshotter.takeSnapshot()
		}
	}
	return zero
}

// TakeSnapshots captures one snapshot per selected instance.
// For groups, snapshots preserve the group's flattened member order.
// For a single state machine instance, it returns a one-element slice.
func TakeSnapshots(ctx context.Context, hsm Instance) []Snapshot {
	if isNilValue(hsm) {
		if current, ok := FromContext(ctx); ok {
			hsm = current
		}
	}
	if isNilValue(hsm) {
		return nil
	}
	if group, ok := hsm.(*Group); ok {
		return group.takeSnapshots()
	}
	snapshot, ok := instanceSnapshot(hsm)
	if !ok {
		return nil
	}
	return []Snapshot{snapshot}
}
