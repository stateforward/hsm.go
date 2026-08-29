package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stateforward/hsm.go"
)

type anyMap map[string]any

type conformanceCase struct {
	Version     string          `json:"version"`
	Name        string          `json:"name"`
	Features    []string        `json:"features"`
	Mode        string          `json:"mode"`
	Model       anyMap          `json:"model"`
	Models      []anyMap        `json:"models"`
	Behaviors   map[string][]op `json:"behaviors"`
	Instances   []anyMap        `json:"instances"`
	Groups      []anyMap        `json:"groups"`
	Script      []op            `json:"script"`
	Expect      anyMap          `json:"expect"`
	Description string          `json:"description"`
}

type op map[string]any

type confInstance struct {
	hsm.HSM
}

type logicalClock struct {
	mutex         sync.Mutex
	now           time.Duration
	registrations []*clockRegistration
	timerName     func() string
}

type clockRegistration struct {
	due   time.Duration
	timer *time.Timer
	name  string
}

type result struct {
	name   string
	status string
	err    error
}

type conformanceError struct {
	code    string
	message string
}

func (e conformanceError) Error() string {
	if e.code == "" {
		return e.message
	}
	return e.code + ": " + e.message
}

var errBehaviorCancelled = errors.New("behavior cancelled")

type exitPointDef struct {
	boundary string
	name     string
	ids      []string
}

type attrSpec struct {
	typ string
}

type dynamicAnyValue struct {
	Value any
}

type deferredEvent struct {
	instanceID string
	eventName  string
	owner      string
}

type queueFanoutGate struct {
	entries []queueGateEntry
}

type queueGateKey struct {
	instanceID string
	eventName  string
}

type queueGateEntry struct {
	key           queueGateKey
	beforeRelease chan struct{}
	afterRelease  chan struct{}
	popSeen       chan struct{}
	claimed       bool
}

type queueGateClaim struct {
	beforeRelease <-chan struct{}
	afterRelease  <-chan struct{}
	popSeen       chan<- struct{}
}

type timerEventDef struct {
	name              string
	kind              string
	transitionOrdinal int
}

type timerIndexBuilder struct {
	r       *runner
	members map[string]bool
}

type submachineTransitionBuckets struct {
	prepended  map[string][]anyMap
	postpended map[string][]anyMap
	root       []anyMap
}

type behaviorScopeKey struct{}
type behaviorStateKey struct{}
type activityStartedKey struct{}
type activityCancelRecorderKey struct{}

type activityCancelRecorder struct {
	mutex     sync.Mutex
	cancelled map[string]bool
}

func (r *activityCancelRecorder) mark(behaviorID string) {
	if r == nil || behaviorID == "" {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.cancelled[behaviorID] = true
}

func (r *activityCancelRecorder) isCancelled(behaviorID string) bool {
	if r == nil || behaviorID == "" {
		return false
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.cancelled[behaviorID]
}

type runner struct {
	caseData           conformanceCase
	models             map[string]*hsm.FinalizedModel
	rawModelIRs        map[string]anyMap
	modelIRs           map[string]anyMap
	attrs              map[string][]string
	scopedAttrs        map[string][]string
	attrSpecs          map[string]attrSpec
	traceSetAttrs      map[string]bool
	operations         map[string]bool
	operationBehaviors map[string]string
	activityExitEvents map[string]map[string]bool
	submachineStates   map[string]bool
	submachineModels   map[string]string
	finalStates        map[string]bool
	exitPoints         map[string]exitPointDef
	deferEvents        map[string]map[string]bool
	timerEventsByOwner map[string][]timerEventDef
	timerNameCache     map[string]string
	timerNameMu        sync.Mutex

	ctx                   context.Context
	cancel                context.CancelFunc
	instances             map[string]*confInstance
	instanceOrder         []string
	instanceQueues        map[string]string
	started               map[string]bool
	ever                  map[string]bool
	startData             map[string]any
	groups                map[string]*hsm.Group
	groupMembers          map[string][]string
	invalidInstanceModels map[string]string
	eventMemory           map[string]hsm.Event
	eventMemoryMu         sync.Mutex
	trace                 []anyMap
	snapshots             map[string]any
	clock                 *logicalClock
	usesConfigClock       bool
	timerMu               sync.Mutex
	pendingTimerScheduled int
	pendingTimerKinds     []string
	pendingTimerKindsByG  map[uint64][]string
	pendingTimerNamesByG  map[uint64][]string
	stableLabel           string
	lastDispatchQueued    bool
	lastError             *conformanceError
	callErrorBaselines    []*conformanceError
	cancelledActivities   map[string]bool
	submachineStack       []string
	pendingDeferred       []deferredEvent
	deferReplayBarrier    bool
	queueGateMu           sync.Mutex
	queueGates            []*queueFanoutGate
	pendingWorkMu         sync.Mutex
	pendingWork           []hsm.Completion
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: conformance [case.json | cases-dir ...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	roots := flag.Args()
	if len(roots) == 0 {
		roots = []string{"../conformance/cases"}
	}
	files, err := collectCaseFiles(roots)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no conformance case files found")
		os.Exit(2)
	}

	var passed, failed int
	for _, file := range files {
		res := runFile(file)
		switch res.status {
		case "PASS":
			passed++
			fmt.Printf("PASS %s\n", res.name)
		default:
			failed++
			fmt.Printf("FAIL %s: %v\n", res.name, res.err)
		}
	}
	fmt.Printf("summary: pass=%d skip=0 fail=%d total=%d\n", passed, failed, len(files))
	if failed > 0 {
		os.Exit(1)
	}
}

func collectCaseFiles(roots []string) ([]string, error) {
	var files []string
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, root)
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(p) != ".json" {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

func runFile(file string) result {
	data, err := os.ReadFile(file)
	if err != nil {
		return result{name: file, status: "FAIL", err: err}
	}
	var c conformanceCase
	if err := json.Unmarshal(data, &c); err != nil {
		return result{name: file, status: "FAIL", err: err}
	}
	name := c.Name
	if name == "" {
		name = filepath.Base(file)
	}
	if c.Version != "hsm-conformance-v1" {
		return result{name: name, status: "FAIL", err: fmt.Errorf("unsupported version %q", c.Version)}
	}
	r := newRunner(c)
	done := make(chan error, 1)
	go func() {
		done <- r.run()
	}()
	timeout := conformanceCaseTimeout()
	select {
	case err := <-done:
		if err != nil {
			return result{name: name, status: "FAIL", err: err}
		}
	case <-time.After(timeout):
		if r.cancel != nil {
			r.cancel()
		}
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
		return result{name: name, status: "FAIL", err: fmt.Errorf("case timeout after %s", timeout)}
	}
	return result{name: name, status: "PASS"}
}

func conformanceCaseTimeout() time.Duration {
	const defaultTimeout = 10 * time.Second
	raw := os.Getenv("HSM_CONFORMANCE_CASE_TIMEOUT_MS")
	if raw == "" {
		return defaultTimeout
	}
	millis, err := strconv.Atoi(raw)
	if err != nil || millis <= 0 {
		return defaultTimeout
	}
	return time.Duration(millis) * time.Millisecond
}

func runnerContextTimeout() time.Duration {
	timeout := conformanceCaseTimeout()
	if timeout > 200*time.Millisecond {
		return timeout - 100*time.Millisecond
	}
	return timeout
}

func newRunner(c conformanceCase) *runner {
	r := &runner{
		caseData:              c,
		models:                map[string]*hsm.FinalizedModel{},
		rawModelIRs:           map[string]anyMap{},
		modelIRs:              map[string]anyMap{},
		attrs:                 map[string][]string{},
		scopedAttrs:           map[string][]string{},
		attrSpecs:             map[string]attrSpec{},
		traceSetAttrs:         map[string]bool{},
		operations:            map[string]bool{},
		operationBehaviors:    map[string]string{},
		activityExitEvents:    map[string]map[string]bool{},
		submachineStates:      map[string]bool{},
		submachineModels:      map[string]string{},
		finalStates:           map[string]bool{},
		exitPoints:            map[string]exitPointDef{},
		deferEvents:           map[string]map[string]bool{},
		timerEventsByOwner:    map[string][]timerEventDef{},
		timerNameCache:        map[string]string{},
		pendingTimerKindsByG:  map[uint64][]string{},
		pendingTimerNamesByG:  map[uint64][]string{},
		instances:             map[string]*confInstance{},
		instanceQueues:        map[string]string{},
		started:               map[string]bool{},
		ever:                  map[string]bool{},
		startData:             map[string]any{},
		groups:                map[string]*hsm.Group{},
		groupMembers:          map[string][]string{},
		invalidInstanceModels: map[string]string{},
		eventMemory:           map[string]hsm.Event{},
		snapshots:             map[string]any{},
		cancelledActivities:   map[string]bool{},
	}
	r.clock = newLogicalClock(r.nextTimerName)
	return r
}

func newLogicalClock(timerName func() string) *logicalClock {
	return &logicalClock{timerName: timerName}
}

func (c *logicalClock) Clock() hsm.Clock {
	return hsm.Clock{
		After:    c.After,
		NewTimer: c.NewTimer,
	}
}

func (c *logicalClock) After(duration time.Duration) <-chan time.Time {
	timer := c.NewTimer(duration)
	return timer.C
}

func (c *logicalClock) NewTimer(duration time.Duration) *time.Timer {
	if duration < 0 {
		duration = 0
	}
	timer := time.NewTimer(24 * time.Hour)
	name := ""
	if c.timerName != nil {
		name = c.timerName()
	}
	c.mutex.Lock()
	c.registrations = append(c.registrations, &clockRegistration{
		due:   c.now + duration,
		timer: timer,
		name:  name,
	})
	c.mutex.Unlock()
	return timer
}

func (c *logicalClock) Advance(duration time.Duration, deliver func(string, func())) {
	if duration < 0 {
		duration = 0
	}
	c.mutex.Lock()
	c.now += duration
	due := make([]*clockRegistration, 0)
	pending := c.registrations[:0]
	for _, registration := range c.registrations {
		if registration.due <= c.now {
			due = append(due, registration)
			continue
		}
		pending = append(pending, registration)
	}
	c.registrations = pending
	c.mutex.Unlock()
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].due != due[j].due {
			return due[i].due < due[j].due
		}
		return timerEventNameLess(due[i].name, due[j].name)
	})
	for _, registration := range due {
		if registration.timer.Stop() {
			trigger := func() {
				registration.timer.Reset(0)
			}
			if deliver != nil && registration.name != "" {
				deliver(registration.name, trigger)
			} else {
				trigger()
				for range 4 {
					runtime.Gosched()
				}
			}
		}
	}
}

func timerEventNameLess(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftOrdinal, leftOK := timerEventTransitionOrdinal(left)
	rightOrdinal, rightOK := timerEventTransitionOrdinal(right)
	if leftOK && rightOK && leftOrdinal != rightOrdinal {
		return leftOrdinal < rightOrdinal
	}
	return left < right
}

func timerEventTransitionOrdinal(eventName string) (int, bool) {
	transitionName := path.Base(path.Dir(eventName))
	if path.Base(eventName) != "duration" && path.Base(eventName) != "timepoint" {
		transitionName = path.Base(path.Dir(path.Dir(eventName)))
	}
	raw, ok := strings.CutPrefix(transitionName, "transition_")
	if !ok {
		return 0, false
	}
	ordinal, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return ordinal, true
}

func (r *runner) run() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recoveredErr, ok := recovered.(error); ok {
				err = fmt.Errorf("panic: %w", recoveredErr)
				return
			}
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	if r.caseData.Mode == "validation" {
		return r.runValidation()
	}
	if _, err := r.buildRuntime(); err != nil {
		return err
	}
	instances := &sync.Map{}
	baseCtx, cancel := context.WithTimeout(context.Background(), runnerContextTimeout())
	r.cancel = cancel
	defer cancel()
	r.ctx = context.WithValue(baseCtx, hsm.Keys.Instances, instances)
	for _, step := range r.caseData.Script {
		if err := r.executeStep(step); err != nil {
			return err
		}
	}
	r.trace = append(r.trace, anyMap{"type": "stable", "state": r.stableState()})
	return r.assertExpectations()
}

func (r *runner) runValidation() error {
	err := r.validationBuildError()
	expected := arrayAny(r.caseData.Expect["validation"])
	if err == nil {
		return fmt.Errorf("validation case unexpectedly built")
	}
	if len(expected) == 0 {
		return nil
	}
	msg := err.Error()
	for _, item := range expected {
		if s, ok := item.(string); ok && strings.Contains(msg, s) {
			return nil
		}
		if m, ok := item.(map[string]any); ok && validationExpectationMatches(m, msg) {
			return nil
		}
		if m, ok := item.(anyMap); ok && validationExpectationMatches(m, msg) {
			return nil
		}
	}
	return fmt.Errorf("validation error mismatch: %q", msg)
}

func (r *runner) validationBuildError() (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recoveredErr, ok := recovered.(error); ok {
				err = fmt.Errorf("panic: %w", recoveredErr)
				return
			}
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	if err := r.validateForBuild(); err != nil {
		return err
	}
	_, err = r.buildRuntime()
	return err
}

func (r *runner) buildRuntime() (*hsm.FinalizedModel, error) {
	model, err := r.buildModels()
	if err != nil {
		return nil, err
	}
	if err := r.buildInstances(model); err != nil {
		return nil, err
	}
	if err := r.buildGroups(); err != nil {
		return nil, err
	}
	return model, nil
}

func validationExpectationMatches(m map[string]any, msg string) bool {
	if contains, _ := m["message_contains"].(string); contains != "" && strings.Contains(msg, contains) {
		return true
	}
	if code, _ := m["code"].(string); code != "" && validationCodeMatches(code, msg) {
		return true
	}
	return false
}

func validationCodeMatches(code, message string) bool {
	if strings.Contains(message, code) {
		return true
	}
	if code == "missing_target" && strings.Contains(message, "must target") {
		return true
	}
	if code == "invalid_entry_point_target" && strings.Contains(message, "entry point target") {
		return true
	}
	checks := map[string]string{
		"missing_initial":                     "missing initial",
		"invalid_name":                        "cannot contain",
		"missing_target":                      "not found",
		"invalid_final_transition":            "cannot",
		"choice_missing_fallback":             "choice_missing_fallback",
		"choice_default_not_last":             "choice_missing_fallback",
		"choice_missing_transition":           "choice_missing_fallback",
		"missing_submachine_model":            "missing_submachine_machine",
		"missing_entry_point":                 "has no entry point",
		"missing_exit_point":                  "has no exit point",
		"invalid_submachine_contents":         "cannot contain nested",
		"invalid_submachine_internal_target":  "cannot target internal state",
		"invalid_submachine_internal_source":  "submachine internal source",
		"invalid_submachine_boundary_target":  "not found",
		"invalid_entry_point_target":          "not found",
		"invalid_entry_point_usage":           "can only target a SubmachineState",
		"invalid_entry_point_internal_target": "entry point target cannot be internal",
		"invalid_entry_point_target_kind":     "entry point target",
		"invalid_exit_point_usage":            "ExitPoint outcome can only be handled",
		"duplicate_model":                     "duplicate model",
		"duplicate_instance":                  "duplicate instance",
		"duplicate_group":                     "duplicate group",
		"duplicate_group_member":              "duplicate group member",
		"unknown_group_member":                "unknown group member",
		"invalid_submachine_initial":          "already has an initial state",
		"submachine_model_cycle":              "recursive submachine model reference",
		"invalid_history_owner":               "within a nested State",
		"missing_operation":                   "missing operation",
		"multiple_transition_triggers":        "multiple transition triggers",
		"multiple_trigger_operands":           "multiple trigger operands",
		"missing_trigger_operand":             "missing trigger operand",
		"invalid_timer_source":                "invalid timer source",
		"missing_source":                      "missing source",
		"history_missing_default":             "requires a default transition",
		"invalid_pseudostate_contents":        "invalid pseudostate contents",
		"empty_event_array":                   "empty event array",
		"extraneous_trigger_operand":          "extraneous trigger operand",
		"invalid_group_cardinality":           "at least two",
		"invalid_behavior_op_operand":         "behavior op",
		"missing_behavior":                    "missing behavior",
		"invalid_attribute":                   "attribute",
		"empty_behavior_array":                "empty behavior array",
		"duplicate_state":                     "already defined",
		"duplicate_entry_point":               "duplicate entry point",
		"duplicate_exit_point":                "duplicate exit point",
		"connection_point_name_collision":     "connection point name collision",
		"missing_attribute":                   "missing attribute",
		"missing_timer_attribute":             "missing timer attribute",
		"invalid_timer_attribute_type":        "invalid timer attribute type",
		"invalid_timer_behavior_return":       "invalid timer behavior return",
	}
	needle := checks[code]
	if needle == "" {
		needle = code
	}
	return strings.Contains(message, needle)
}

func (r *runner) validateForBuild() error {
	modelIRs := r.validationModelIRs()
	if err := resolveModelIRMap(modelIRs); err != nil {
		return err
	}
	names := make([]string, 0, len(modelIRs))
	for name := range modelIRs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		modelIR := modelIRs[name]
		if err := r.validateAttributes(modelIR); err != nil {
			return err
		}
		if err := r.validateIRShape(modelIR); err != nil {
			return err
		}
		if err := r.validateTriggerOperands(modelIR); err != nil {
			return err
		}
		if err := r.validateTransitionPaths(modelIR); err != nil {
			return err
		}
	}
	mainName, _ := r.caseData.Model["name"].(string)
	mainIR := modelIRs[mainName]
	if mainIR == nil {
		mainIR = r.caseData.Model
	}
	if err := r.validateOnCallOperations(mainIR, nil, map[string]bool{}, modelIRs); err != nil {
		return err
	}
	if err := r.validateBehaviorPrograms(); err != nil {
		return err
	}
	if err := r.validateInstances(); err != nil {
		return err
	}
	if err := r.validateGroups(); err != nil {
		return err
	}
	return nil
}

func (r *runner) validationModelIRs() map[string]anyMap {
	modelIRs := map[string]anyMap{}
	if name, ok := r.caseData.Model["name"].(string); ok && name != "" {
		modelIRs[name] = cloneObject(r.caseData.Model)
	}
	for _, modelIR := range r.caseData.Models {
		if name, ok := modelIR["name"].(string); ok && name != "" {
			modelIRs[name] = cloneObject(modelIR)
		}
	}
	return modelIRs
}

func (r *runner) validateAttributes(modelIR anyMap) error {
	for attrName, specAny := range object(modelIR["attributes"]) {
		if strings.Contains(attrName, "/") {
			return fmt.Errorf("invalid_name: attribute name %q cannot contain \"/\"", attrName)
		}
		spec := object(specAny)
		if spec == nil {
			return fmt.Errorf("invalid_attribute: attribute %q declaration must be an object", attrName)
		}
		typ, hasType := spec["type"].(string)
		value, hasDefault := spec["default"]
		if !hasType && !hasDefault {
			return fmt.Errorf("invalid_attribute: attribute %q requires type or default", attrName)
		}
		if hasType && hasDefault && !valueMatchesAttrType(value, typ) {
			return fmt.Errorf("invalid_attribute: attribute %q default does not match declared type", attrName)
		}
	}
	for opName := range object(modelIR["operations"]) {
		if strings.Contains(opName, "/") {
			return fmt.Errorf("invalid_name: operation name %q cannot contain \"/\"", opName)
		}
	}
	return nil
}

func (r *runner) validateIRShape(modelIR anyMap) error {
	var behaviorArray = func(parent anyMap, field string) error {
		if value, ok := parent[field]; ok {
			if arr, ok := value.([]any); ok && len(arr) == 0 {
				return fmt.Errorf("empty_behavior_array: %s", field)
			}
		}
		return nil
	}
	var transitionArrays = func(transition anyMap) error {
		return behaviorArray(transition, "effects")
	}
	modelName, _ := modelIR["name"].(string)
	rootPath := "/" + modelName
	var walkState func(anyMap, string) error
	walkState = func(state anyMap, ownerPath string) error {
		for _, field := range []string{"entry", "exit", "activity"} {
			if err := behaviorArray(state, field); err != nil {
				return err
			}
		}
		if value, ok := state["defer"]; ok {
			for _, rawEvent := range arrayAny(value) {
				if _, err := eventNameValue(rawEvent); err != nil {
					return err
				}
			}
		}
		kind, _ := state["kind"].(string)
		if kind == "submachine" && len(arrayAny(state["states"])) > 0 {
			return fmt.Errorf("invalid_submachine_contents: submachine state cannot contain nested states")
		}
		if kind == "choice" || kind == "shallow_history" || kind == "deep_history" {
			if _, ok := state["initial"]; ok {
				return fmt.Errorf("invalid_submachine_initial: already has an initial state")
			}
			if (kind == "shallow_history" || kind == "deep_history") && path.Dir(ownerPath) == rootPath {
				return fmt.Errorf("invalid_history_owner: history pseudostate must be within a nested State")
			}
			for _, field := range []string{"entry", "exit", "activity", "defer", "states", "initial"} {
				if _, ok := state[field]; ok {
					return fmt.Errorf("invalid_pseudostate_contents")
				}
			}
			if (kind == "shallow_history" || kind == "deep_history") && len(arrayAny(state["transitions"])) == 0 {
				return fmt.Errorf("history_missing_default: history requires a default transition")
			}
		}
		if kind == "final" {
			for _, field := range []string{"entry", "exit", "activity", "defer", "states", "initial", "transitions"} {
				if _, ok := state[field]; ok {
					return fmt.Errorf("invalid_final_transition: final states cannot declare %s", field)
				}
			}
		}
		if initial := object(state["initial"]); initial != nil {
			if err := behaviorArray(initial, "effects"); err != nil {
				return err
			}
		}
		for _, transitionAny := range arrayAny(state["transitions"]) {
			if transition := object(transitionAny); transition != nil {
				if err := transitionArrays(transition); err != nil {
					return err
				}
			}
		}
		children := arrayAny(state["states"])
		if len(children) > 0 {
			if _, ok := state["initial"]; !ok {
				return fmt.Errorf("missing_initial")
			}
		}
		seenChildren := map[string]bool{}
		for _, childAny := range children {
			if child := object(childAny); child != nil {
				childName, _ := child["name"].(string)
				if childName != "" {
					if seenChildren[childName] {
						return fmt.Errorf("duplicate_state: state %q already defined", childName)
					}
					seenChildren[childName] = true
				}
				childPath := path.Join(ownerPath, childName)
				if err := walkState(child, childPath); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if initial := object(modelIR["initial"]); initial != nil {
		if err := behaviorArray(initial, "effects"); err != nil {
			return err
		}
	}
	for _, field := range []string{"entry_points", "exit_points"} {
		for _, pointAny := range arrayAny(modelIR[field]) {
			if point := object(pointAny); point != nil {
				if err := behaviorArray(point, "effects"); err != nil {
					return err
				}
			}
		}
	}
	for _, transitionAny := range arrayAny(modelIR["transitions"]) {
		if transition := object(transitionAny); transition != nil {
			if err := transitionArrays(transition); err != nil {
				return err
			}
		}
	}
	if _, ok := modelIR["initial"]; !ok {
		return fmt.Errorf("missing initial")
	}
	seenStates := map[string]bool{}
	for _, stateAny := range arrayAny(modelIR["states"]) {
		if state := object(stateAny); state != nil {
			stateName, _ := state["name"].(string)
			if stateName != "" {
				if seenStates[stateName] {
					return fmt.Errorf("duplicate_state: state %q already defined", stateName)
				}
				seenStates[stateName] = true
			}
			statePath := path.Join(rootPath, stateName)
			if err := walkState(state, statePath); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) validateTransitionPaths(modelIR anyMap) error {
	modelName, _ := modelIR["name"].(string)
	rootPath := "/" + modelName
	statePaths := map[string]bool{}
	submachines := map[string]bool{}
	stateKinds := map[string]string{}
	topStateNames := map[string]bool{}
	var walk func([]any, string)
	walk = func(states []any, ownerPath string) {
		for _, stateAny := range states {
			state := object(stateAny)
			if state == nil {
				continue
			}
			name, _ := state["name"].(string)
			if name == "" {
				continue
			}
			statePath := path.Join(ownerPath, name)
			statePaths[statePath] = true
			kind, _ := state["kind"].(string)
			if kind == "" {
				kind = "state"
			}
			stateKinds[statePath] = kind
			if ownerPath == rootPath {
				topStateNames[name] = true
			}
			if kind == "submachine" {
				submachines[statePath] = true
			}
			walk(arrayAny(state["states"]), statePath)
		}
	}
	walk(arrayAny(modelIR["states"]), rootPath)
	pathPrefixes := func(candidate string) []string {
		parts := strings.Split(strings.Trim(candidate, "/"), "/")
		prefixes := make([]string, 0, len(parts))
		for index := range parts {
			if parts[index] == "" {
				continue
			}
			prefixes = append(prefixes, "/"+strings.Join(parts[:index+1], "/"))
		}
		return prefixes
	}
	isInternalSubmachinePath := func(candidate string) bool {
		prefixes := pathPrefixes(candidate)
		for _, prefix := range prefixes[:max(0, len(prefixes)-1)] {
			if stateKinds[prefix] == "submachine" {
				return true
			}
		}
		return false
	}
	validateStateTarget := func(targetPath string) error {
		if !statePaths[targetPath] {
			return fmt.Errorf("target %q not found", targetPath)
		}
		return nil
	}
	validateInitial := func(initial any, ownerPath string) error {
		target := initial
		if initialObj := object(initial); initialObj != nil {
			target = initialObj["target"]
		}
		targetString, err := requireStringValue(target)
		if err != nil {
			return err
		}
		return validateStateTarget(resolveInitialTarget(targetString, ownerPath, rootPath, rootPath))
	}
	validateTransitionTarget := func(raw any, ownerPath string, bareTargets bool, hasEntryPoint bool) error {
		if raw == nil {
			return nil
		}
		target, err := requireStringValue(raw)
		if err != nil {
			return err
		}
		if strings.HasPrefix(target, ".entry/") || strings.HasPrefix(target, ".exit/") {
			return fmt.Errorf("invalid_entry_point_internal_target: entry point target cannot be internal")
		}
		for _, pointAny := range arrayAny(modelIR["entry_points"]) {
			point := object(pointAny)
			name, _ := point["name"].(string)
			if name != "" && (target == name || path.Clean(target) == path.Join(rootPath, name)) {
				return fmt.Errorf("invalid_entry_point_internal_target: entry point target cannot be internal")
			}
		}
		targetPath := resolvePathInScope(target, ownerPath, bareTargets, rootPath, rootPath)
		if isInternalSubmachinePath(targetPath) {
			return fmt.Errorf("invalid_submachine_internal_target: cannot target internal state %s", targetPath)
		}
		if hasEntryPoint && stateKinds[targetPath] != "submachine" {
			return fmt.Errorf("invalid_entry_point_usage: entry point can only target a SubmachineState")
		}
		return validateStateTarget(targetPath)
	}
	validateTransition := func(transition anyMap, ownerPath string, bareTargets bool) error {
		sourcePath := ""
		if rawSource, ok := transition["source"]; ok {
			source, err := requireStringValue(rawSource)
			if err != nil {
				return err
			}
			sourcePath = resolvePathInScope(source, ownerPath, bareTargets, rootPath, rootPath)
			if isInternalSubmachinePath(sourcePath) {
				return fmt.Errorf("invalid_submachine_internal_source: submachine internal source %s", sourcePath)
			}
			if !statePaths[sourcePath] {
				return fmt.Errorf("missing_source: missing source %q", sourcePath)
			}
		}
		if rawTarget, ok := transition["target"]; ok {
			if target, _ := rawTarget.(string); target == "." && sourcePath != "" {
				if isInternalSubmachinePath(sourcePath) {
					return fmt.Errorf("invalid_submachine_internal_target: cannot target internal state %s", sourcePath)
				}
				return validateStateTarget(sourcePath)
			}
			_, hasEntryPoint := transition["entry_point"].(string)
			return validateTransitionTarget(rawTarget, ownerPath, bareTargets, hasEntryPoint)
		}
		return nil
	}
	var walkTransitions func([]any, string) error
	walkTransitions = func(states []any, ownerPath string) error {
		for _, stateAny := range states {
			state := object(stateAny)
			if state == nil {
				continue
			}
			name, _ := state["name"].(string)
			statePath := path.Join(ownerPath, name)
			kind, _ := state["kind"].(string)
			children := arrayAny(state["states"])
			if len(children) > 0 && (kind == "" || kind == "state") {
				initial, ok := state["initial"]
				if !ok {
					return fmt.Errorf("missing initial")
				}
				if err := validateInitial(initial, statePath); err != nil {
					return err
				}
			}
			bareTargets := kind == "choice" || kind == "shallow_history" || kind == "deep_history"
			transitionOwner := statePath
			if bareTargets {
				transitionOwner = ownerPath
			}
			for _, transitionAny := range arrayAny(state["transitions"]) {
				if transition := object(transitionAny); transition != nil {
					if err := validateTransition(transition, transitionOwner, bareTargets); err != nil {
						return err
					}
				}
			}
			if err := walkTransitions(children, statePath); err != nil {
				return err
			}
		}
		return nil
	}
	if initial, ok := modelIR["initial"]; ok {
		if err := validateInitial(initial, rootPath); err != nil {
			return err
		}
	}
	entryPoints := map[string]bool{}
	exitPoints := map[string]bool{}
	for _, pointAny := range arrayAny(modelIR["entry_points"]) {
		point := object(pointAny)
		if point == nil {
			continue
		}
		name, err := requireString(point, "name")
		if err != nil {
			return err
		}
		if entryPoints[name] {
			return fmt.Errorf("duplicate entry point %q", name)
		}
		if topStateNames[name] {
			return fmt.Errorf("connection point name collision %q", name)
		}
		entryPoints[name] = true
	}
	for _, pointAny := range arrayAny(modelIR["exit_points"]) {
		point := object(pointAny)
		if point == nil {
			continue
		}
		name, err := requireString(point, "name")
		if err != nil {
			return err
		}
		if exitPoints[name] {
			return fmt.Errorf("duplicate exit point %q", name)
		}
		if topStateNames[name] {
			return fmt.Errorf("connection point name collision %q", name)
		}
		exitPoints[name] = true
	}
	for _, pointAny := range arrayAny(modelIR["entry_points"]) {
		point := object(pointAny)
		if point == nil {
			continue
		}
		rawTarget, err := requireString(point, "target")
		if err != nil {
			return err
		}
		if entryPoints[rawTarget] || exitPoints[rawTarget] {
			return fmt.Errorf("entry point target %q is not a state", rawTarget)
		}
		targetPath := resolvePathInScope(rawTarget, rootPath, false, rootPath, rootPath)
		if !statePaths[targetPath] {
			return fmt.Errorf("target %q not found", targetPath)
		}
	}
	for _, transitionAny := range arrayAny(modelIR["transitions"]) {
		if transition := object(transitionAny); transition != nil {
			if err := validateTransition(transition, rootPath, false); err != nil {
				return err
			}
		}
	}
	return walkTransitions(arrayAny(modelIR["states"]), rootPath)
}

func (r *runner) validateBehaviorPrograms() error {
	required := map[string]map[string]bool{
		"trace":                             {"value": true},
		"set_attr":                          {"name": true, "value": true},
		"set_attr_from_event_data":          {"name": true, "path": true},
		"get_attr":                          {"name": true},
		"return_attr":                       {"name": true},
		"return_value":                      {"value": true},
		"return_equals":                     {"name": true, "value": true},
		"event_name_equals":                 {"value": true},
		"event_data_equals":                 {"path": true, "value": true},
		"event_data_get":                    {"path": true},
		"event_application_metadata_equals": {"name": true, "value": true},
		"event_metadata_set":                {"name": true, "value": true},
		"event_metadata_get":                {"name": true},
		"event_metadata_equals":             {"name": true, "value": true},
		"dispatch":                          {"event": true},
		"call":                              {"name": true},
		"sleep":                             {"millis": true},
		"snapshot":                          {},
		"yield":                             {},
	}
	allowed := map[string]map[string]bool{}
	for kind, keys := range required {
		allowed[kind] = map[string]bool{"op": true}
		for key := range keys {
			allowed[kind][key] = true
		}
	}
	allowed["dispatch"] = map[string]bool{"op": true, "event": true, "target": true, "instance": true, "group": true}
	allowed["raise"] = map[string]bool{"op": true, "event": true, "code": true, "value": true}
	for behaviorID, program := range r.caseData.Behaviors {
		if len(program) == 0 {
			return fmt.Errorf("missing_behavior: missing behavior program %q", behaviorID)
		}
		for index, step := range program {
			kind, ok := step["op"].(string)
			if !ok || kind == "" {
				return fmt.Errorf("invalid_behavior_op_operand: behavior op %s[%d] must declare op", behaviorID, index)
			}
			if kind == "raise" {
				hasEvent := step["event"] != nil
				hasCode := step["code"] != nil
				if hasEvent == hasCode {
					return fmt.Errorf("invalid_behavior_op_operand: behavior op %s[%d] raise requires exactly one of event or code", behaviorID, index)
				}
				for key := range step {
					if !allowed["raise"][key] {
						return fmt.Errorf("invalid_behavior_op_operand: behavior op %s[%d] raise has unsupported operand %q", behaviorID, index, key)
					}
				}
				continue
			}
			requiredKeys, ok := required[kind]
			if !ok {
				return fmt.Errorf("invalid_behavior_op_operand: unsupported behavior op %q", kind)
			}
			if kind == "dispatch" && boolCount(step["target"] != nil, step["instance"] != nil, step["group"] != nil) > 1 {
				return fmt.Errorf("invalid_behavior_op_operand: behavior op %s[%d] dispatch can declare only one target selector", behaviorID, index)
			}
			if (kind == "dispatch" || kind == "raise") && step["event"] != nil {
				if _, err := eventFromValue(step["event"]); err != nil {
					return fmt.Errorf("invalid_behavior_op_operand: behavior op %s[%d] %s event: %w", behaviorID, index, kind, err)
				}
			}
			for key := range requiredKeys {
				if _, ok := step[key]; !ok {
					return fmt.Errorf("invalid_behavior_op_operand: behavior op %s[%d] %s missing operand %q", behaviorID, index, kind, key)
				}
			}
			for key := range step {
				if !allowed[kind][key] {
					return fmt.Errorf("invalid_behavior_op_operand: behavior op %s[%d] %s has unsupported operand %q", behaviorID, index, kind, key)
				}
			}
		}
	}
	return nil
}

func (r *runner) validateTriggerOperands(modelIR anyMap) error {
	attrTypes := map[string]string{}
	for attrName, specAny := range object(modelIR["attributes"]) {
		spec := object(specAny)
		typ, _ := spec["type"].(string)
		if typ == "" {
			typ = inferAttrType(spec["default"])
		}
		attrTypes[attrName] = typ
	}
	allowedByKind := map[string]map[string]bool{
		"on":         {"kind": true, "event": true, "events": true},
		"on_set":     {"kind": true, "attribute": true},
		"on_call":    {"kind": true, "operation": true},
		"when":       {"kind": true, "attribute": true, "behavior": true},
		"completion": {"kind": true},
		"exit_point": {"kind": true, "exit_point": true},
		"after":      {"kind": true, "duration_ms": true, "time_ms": true, "attribute": true, "behavior": true},
		"every":      {"kind": true, "duration_ms": true, "time_ms": true, "attribute": true, "behavior": true},
		"at":         {"kind": true, "duration_ms": true, "time_ms": true, "attribute": true, "behavior": true},
	}
	validateTransition := func(transition anyMap) error {
		if transition["target"] == nil && transition["entry_point"] == nil && len(arrayAny(transition["effects"])) == 0 {
			if transition["on"] != nil || transition["trigger"] != nil || transition["guard"] != nil {
				return fmt.Errorf("missing_target: transition requires target or effect")
			}
		}
		if _, hasOn := transition["on"]; hasOn {
			if _, hasTrigger := transition["trigger"]; hasTrigger {
				return fmt.Errorf("multiple_transition_triggers")
			}
		}
		trigger := object(transition["trigger"])
		if trigger == nil {
			return nil
		}
		kind, _ := trigger["kind"].(string)
		allowed := allowedByKind[kind]
		if allowed == nil {
			return nil
		}
		for key := range trigger {
			if !allowed[key] {
				return fmt.Errorf("extraneous_trigger_operand: %s", key)
			}
		}
		switch kind {
		case "on":
			hasEvent := trigger["event"] != nil
			hasEvents := trigger["events"] != nil
			if !hasEvent && !hasEvents {
				return fmt.Errorf("missing_trigger_operand")
			}
			if hasEvent && hasEvents {
				return fmt.Errorf("multiple_trigger_operands")
			}
			if hasEvent {
				if _, err := eventNameValue(trigger["event"]); err != nil {
					return err
				}
			}
			if hasEvents {
				for _, rawEvent := range arrayAny(trigger["events"]) {
					if _, err := eventNameValue(rawEvent); err != nil {
						return err
					}
				}
			}
		case "on_set", "on_call", "exit_point":
			field := map[string]string{"on_set": "attribute", "on_call": "operation", "exit_point": "exit_point"}[kind]
			value, ok := trigger[field]
			if !ok {
				return fmt.Errorf("missing_trigger_operand")
			}
			if s, ok := value.(string); ok && strings.Contains(s, "/") {
				return fmt.Errorf("invalid_name: %s name %q cannot contain \"/\"", field, s)
			}
			if kind == "on_set" {
				if s, ok := value.(string); ok && s != "" {
					if _, ok := attrTypes[s]; !ok {
						return fmt.Errorf("missing_attribute: %s", s)
					}
				}
			}
		case "when":
			hasAttribute := trigger["attribute"] != nil
			hasBehavior := trigger["behavior"] != nil
			if !hasAttribute && !hasBehavior {
				return fmt.Errorf("missing_trigger_operand")
			}
			if hasAttribute && hasBehavior {
				return fmt.Errorf("multiple_trigger_operands")
			}
			if s, ok := trigger["attribute"].(string); ok && strings.Contains(s, "/") {
				return fmt.Errorf("invalid_name: attribute name %q cannot contain \"/\"", s)
			}
			if s, ok := trigger["attribute"].(string); ok && s != "" {
				if _, ok := attrTypes[s]; !ok {
					return fmt.Errorf("missing_attribute: %s", s)
				}
			}
			if s, ok := trigger["behavior"].(string); ok && s != "" {
				if _, ok := r.caseData.Behaviors[s]; !ok {
					return fmt.Errorf("missing_behavior: %s", s)
				}
			}
		case "after", "every":
			count := boolCount(trigger["duration_ms"] != nil, trigger["time_ms"] != nil, trigger["attribute"] != nil, trigger["behavior"] != nil)
			if count != 1 || trigger["time_ms"] != nil {
				return fmt.Errorf("invalid_timer_source")
			}
			if kind == "every" && durationMillis(trigger["duration_ms"]) == 0 && trigger["duration_ms"] != nil {
				return fmt.Errorf("invalid_timer_source")
			}
			if err := r.validateTimerTriggerSource(kind, trigger, attrTypes); err != nil {
				return err
			}
		case "at":
			count := boolCount(trigger["duration_ms"] != nil, trigger["time_ms"] != nil, trigger["attribute"] != nil, trigger["behavior"] != nil)
			if count != 1 || trigger["duration_ms"] != nil {
				return fmt.Errorf("invalid_timer_source")
			}
			if err := r.validateTimerTriggerSource(kind, trigger, attrTypes); err != nil {
				return err
			}
		}
		return nil
	}
	return walkModelTransitions(modelIR, validateTransition, nil)
}

func (r *runner) validateTimerTriggerSource(kind string, trigger anyMap, attrTypes map[string]string) error {
	if attr, ok := trigger["attribute"].(string); ok && attr != "" {
		typ, exists := attrTypes[attr]
		if !exists {
			return fmt.Errorf("missing_timer_attribute: %s", attr)
		}
		if (kind == "at" && typ != "time_ms") || (kind != "at" && typ == "time_ms") {
			return fmt.Errorf("invalid_timer_attribute_type: %s", attr)
		}
	}
	if behavior, ok := trigger["behavior"].(string); ok && behavior != "" {
		program, exists := r.caseData.Behaviors[behavior]
		if !exists {
			return fmt.Errorf("missing_behavior: %s", behavior)
		}
		if !timerBehaviorCanReturnDuration(program) {
			return fmt.Errorf("invalid_timer_behavior_return: %s", behavior)
		}
	}
	return nil
}

func timerBehaviorCanReturnDuration(program []op) bool {
	for _, step := range program {
		switch step["op"] {
		case "return_value":
			switch normalizeJSONValue(step["value"]).(type) {
			case int, int64, float64, json.Number:
				return true
			default:
				return false
			}
		case "return_attr", "event_data_get":
			return true
		}
	}
	return true
}

func walkModelTransitions(modelIR anyMap, visitTransition func(anyMap) error, visitState func(anyMap) error) error {
	visitTransitions := func(transitions []any) error {
		for _, transitionAny := range transitions {
			if transition := object(transitionAny); transition != nil {
				if err := visitTransition(transition); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visitTransitions(arrayAny(modelIR["transitions"])); err != nil {
		return err
	}
	var walkStates func([]any) error
	walkStates = func(states []any) error {
		for _, stateAny := range states {
			state := object(stateAny)
			if state == nil {
				continue
			}
			if err := visitTransitions(arrayAny(state["transitions"])); err != nil {
				return err
			}
			if visitState != nil {
				if err := visitState(state); err != nil {
					return err
				}
			}
			if err := walkStates(arrayAny(state["states"])); err != nil {
				return err
			}
		}
		return nil
	}
	return walkStates(arrayAny(modelIR["states"]))
}

func (r *runner) validateOnCallOperations(modelIR anyMap, inherited map[string]bool, seen map[string]bool, modelIRs map[string]anyMap) error {
	modelName, _ := modelIR["name"].(string)
	if modelName == "" || seen[modelName] {
		return nil
	}
	seen[modelName] = true
	visible := map[string]bool{}
	for name := range inherited {
		visible[name] = true
	}
	for name := range object(modelIR["operations"]) {
		visible[name] = true
	}
	validateTransition := func(transition anyMap) error {
		trigger := object(transition["trigger"])
		if trigger == nil || trigger["kind"] != "on_call" {
			return nil
		}
		operation, ok := trigger["operation"].(string)
		if ok && operation != "" && !visible[operation] {
			return fmt.Errorf("missing_operation: %s", operation)
		}
		return nil
	}
	visitState := func(state anyMap) error {
		if kind, _ := state["kind"].(string); kind != "submachine" {
			return nil
		}
		childName, _ := state["machine"].(string)
		childIR := modelIRs[childName]
		if childIR == nil {
			return nil
		}
		return r.validateOnCallOperations(childIR, visible, seen, modelIRs)
	}
	return walkModelTransitions(modelIR, validateTransition, visitState)
}

func (r *runner) validateInstances() error {
	seen := map[string]bool{}
	for _, instanceIR := range r.caseData.Instances {
		id, err := requireString(instanceIR, "id")
		if err != nil {
			return err
		}
		if seen[id] {
			return fmt.Errorf("duplicate_instance: %q", id)
		}
		seen[id] = true
	}
	return nil
}

func (r *runner) validateGroups() error {
	instances := map[string]bool{}
	if len(r.caseData.Instances) == 0 {
		instances["default"] = true
	}
	for _, instanceIR := range r.caseData.Instances {
		id, err := requireString(instanceIR, "id")
		if err != nil {
			return err
		}
		instances[id] = true
	}
	if err := r.requireUniqueGroupIDs(); err != nil {
		return err
	}
	for _, groupIR := range r.caseData.Groups {
		membersValue, ok := groupIR["members"].([]any)
		if !ok {
			return fmt.Errorf("group.members must be an array")
		}
		seenMembers := map[string]bool{}
		for _, memberAny := range membersValue {
			memberID, err := memberIDValue(memberAny)
			if err != nil {
				return err
			}
			if seenMembers[memberID] {
				return fmt.Errorf("duplicate_group_member: %q", memberID)
			}
			if !instances[memberID] {
				return fmt.Errorf("unknown_group_member: %q", memberID)
			}
			seenMembers[memberID] = true
		}
		if len(membersValue) < 2 {
			return fmt.Errorf("invalid_group_cardinality: group must contain at least two members")
		}
	}
	return nil
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func (r *runner) buildModels() (*hsm.FinalizedModel, error) {
	mainName, err := requireString(r.caseData.Model, "name")
	if err != nil {
		return nil, err
	}
	if strings.Contains(mainName, "/") {
		return nil, fmt.Errorf("invalid_name: model name %q", mainName)
	}
	r.rawModelIRs[mainName] = cloneObject(r.caseData.Model)
	r.modelIRs[mainName] = cloneObject(r.caseData.Model)
	for _, modelIR := range r.caseData.Models {
		name, err := requireString(modelIR, "name")
		if err != nil {
			return nil, err
		}
		if strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid_name: model name %q", name)
		}
		if _, exists := r.rawModelIRs[name]; exists {
			return nil, fmt.Errorf("duplicate_model: %q", name)
		}
		r.rawModelIRs[name] = cloneObject(modelIR)
		r.modelIRs[name] = cloneObject(modelIR)
	}
	if err := r.resolveModelIRs(); err != nil {
		return nil, err
	}
	for _, modelIR := range r.modelIRs {
		for _, refAny := range object(modelIR["operations"]) {
			if _, err := r.requireBehaviorID(refAny); err != nil {
				return nil, err
			}
		}
	}
	mainModelIR := r.modelIRs[mainName]
	r.indexTimerEvents(mainModelIR)
	r.collectSubmachineStates(mainModelIR, "/"+mainName, "/"+mainName, "/"+mainName)
	return r.buildModel(r.rawModelIRs[mainName])
}

func (r *runner) resolveModelIRs() error {
	return resolveModelIRMap(r.modelIRs)
}

func resolveModelIRMap(modelIRs map[string]anyMap) error {
	names := make([]string, 0, len(modelIRs))
	for name := range modelIRs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		modelIR := modelIRs[name]
		if _, ok := modelIR["redefines"]; !ok {
			continue
		}
		merged, err := redefinedModelIRFrom(modelIRs, modelIR, map[string]bool{})
		if err != nil {
			return err
		}
		modelIRs[name] = merged
	}
	return nil
}

func (r *runner) indexTimerEvents(modelIR anyMap) {
	r.timerEventsByOwner = map[string][]timerEventDef{}
	name, _ := modelIR["name"].(string)
	if name == "" {
		return
	}
	builder := &timerIndexBuilder{r: r, members: map[string]bool{}}
	builder.indexModel(modelIR, "/"+name, "/"+name, "/"+name, map[string]bool{})
}

func (b *timerIndexBuilder) addMember(name string) {
	if name == "" || b.members[name] {
		return
	}
	b.members[name] = true
}

func (b *timerIndexBuilder) memberCount() int {
	return len(b.members)
}

func (b *timerIndexBuilder) indexModel(modelIR anyMap, ownerPath, sourceRoot, targetRoot string, seen map[string]bool) {
	b.addMember(ownerPath)
	if _, ok := modelIR["initial"]; ok {
		b.addMember(path.Join(ownerPath, ".initial"))
		b.addMember(path.Join(ownerPath, ".initial", "initial"))
	}
	for _, stateAny := range arrayAny(modelIR["states"]) {
		b.indexState(object(stateAny), ownerPath, sourceRoot, targetRoot, nil, nil, seen)
	}
	transitionOrdinal := 0
	for transitionIndex, transitionAny := range arrayAny(modelIR["transitions"]) {
		transitionIR := object(transitionAny)
		if transitionIR == nil {
			continue
		}
		if rootTransitionRequiresExpansion(transitionIR) {
			expanded, err := rootTransitionExpansions(modelIR, ownerPath, transitionIR, transitionIndex)
			if err != nil {
				b.addMember(path.Join(ownerPath, "invalid_root_transition"))
				continue
			}
			for _, expandedTransition := range expanded {
				transitionOrdinal++
				b.indexTransition(expandedTransition, ownerPath, transitionOrdinal)
			}
			continue
		}
		transitionOrdinal++
		b.indexTransition(transitionIR, ownerPath, transitionOrdinal)
	}
}

func (b *timerIndexBuilder) indexState(stateIR anyMap, ownerPath, sourceRoot, targetRoot string, prependedTransitions, postpendedTransitions map[string][]anyMap, seen map[string]bool) {
	if stateIR == nil {
		return
	}
	name, _ := stateIR["name"].(string)
	if name == "" {
		return
	}
	statePath := path.Join(ownerPath, name)
	b.addMember(statePath)
	if _, ok := stateIR["initial"]; ok {
		b.addMember(path.Join(statePath, ".initial"))
		b.addMember(path.Join(statePath, ".initial", "initial"))
	}
	for _, field := range []string{"entry", "exit", "activity"} {
		for index := range arrayAny(stateIR[field]) {
			b.addMember(path.Join(statePath, field, strconv.Itoa(index)))
		}
	}
	kindName, _ := stateIR["kind"].(string)
	transitionOwner := statePath
	bareTargets := false
	if kindName == "choice" || kindName == "shallow_history" || kindName == "deep_history" {
		transitionOwner = ownerPath
		bareTargets = true
	}
	if kindName == "submachine" {
		b.indexSubmachine(stateIR, statePath, seen)
	} else {
		for _, child := range arrayAny(stateIR["states"]) {
			b.indexState(object(child), statePath, sourceRoot, targetRoot, prependedTransitions, postpendedTransitions, seen)
		}
	}
	transitionOrdinal := 0
	for _, transition := range prependedTransitions[statePath] {
		transitionOrdinal++
		b.indexTransition(transition, transitionOwner, transitionOrdinal)
	}
	for _, transition := range arrayAny(stateIR["transitions"]) {
		_ = bareTargets
		transitionOrdinal++
		b.indexTransition(object(transition), transitionOwner, transitionOrdinal)
	}
	for _, transition := range postpendedTransitions[statePath] {
		transitionOrdinal++
		b.indexTransition(transition, transitionOwner, transitionOrdinal)
	}
}

func (b *timerIndexBuilder) indexSubmachine(stateIR anyMap, statePath string, seen map[string]bool) {
	machineName, _ := stateIR["machine"].(string)
	childModel := b.r.modelIRs[machineName]
	if childModel == nil || seen[machineName] {
		return
	}
	seen[machineName] = true
	defer delete(seen, machineName)
	if _, ok := childModel["initial"]; ok {
		b.addMember(path.Join(statePath, ".initial"))
		b.addMember(path.Join(statePath, ".initial", "initial"))
	}
	childRoot := "/" + machineName
	buckets, _ := partitionSubmachineTransitions(arrayAny(childModel["transitions"]), statePath, childRoot, false)
	for _, child := range arrayAny(childModel["states"]) {
		b.indexState(object(child), statePath, childRoot, statePath, buckets.prepended, buckets.postpended, seen)
	}
	transitionOrdinal := 0
	for _, transition := range buckets.root {
		transitionOrdinal++
		b.indexTransition(transition, statePath, transitionOrdinal)
	}
}

func partitionSubmachineTransitions(transitions []any, statePath, childRoot string, strict bool) (submachineTransitionBuckets, error) {
	buckets := submachineTransitionBuckets{
		prepended:  map[string][]anyMap{},
		postpended: map[string][]anyMap{},
		root:       make([]anyMap, 0),
	}
	for _, transition := range transitions {
		transitionIR := object(transition)
		if transitionIR == nil {
			continue
		}
		if rawSource, ok := transitionIR["source"]; ok {
			source, err := requireStringValue(rawSource)
			if err != nil {
				if strict {
					return buckets, err
				}
				continue
			}
			sourcePath := resolvePathInScope(source, statePath, false, childRoot, statePath)
			if isExitPointTrigger(transitionIR) && transitionIR["guard"] == nil {
				buckets.postpended[sourcePath] = append(buckets.postpended[sourcePath], transitionIR)
			} else {
				buckets.prepended[sourcePath] = append(buckets.prepended[sourcePath], transitionIR)
			}
			continue
		}
		buckets.root = append(buckets.root, transitionIR)
	}
	return buckets, nil
}

func (b *timerIndexBuilder) indexTransition(transitionIR anyMap, ownerPath string, transitionOrdinal int) {
	if transitionIR == nil {
		return
	}
	transitionName := ""
	if id, ok := transitionIR["id"].(string); ok && id != "" {
		transitionName = path.Join(ownerPath, id)
	} else {
		transitionName = path.Join(ownerPath, fmt.Sprintf("transition_%d", b.memberCount()))
	}
	b.addMember(transitionName)
	trigger := object(transitionIR["trigger"])
	if trigger == nil {
		if on, ok := transitionIR["on"]; ok {
			trigger = anyMap{"kind": "on", "event": on}
		}
	}
	kindName, _ := trigger["kind"].(string)
	isTimerTrigger := false
	switch kindName {
	case "after", "every", "at":
		isTimerTrigger = true
		timerPart := timerPartForKind(kindName)
		if timerPart != "" {
			name := path.Join(transitionName, timerPart)
			b.r.timerEventsByOwner[ownerPath] = append(b.r.timerEventsByOwner[ownerPath], timerEventDef{
				name:              name,
				kind:              kindName,
				transitionOrdinal: transitionOrdinal,
			})
		}
	}
	if isTimerTrigger || transitionIR["guard"] != nil {
		b.addMember(path.Join(transitionName, "guard"))
	}
	for index := range arrayAny(transitionIR["effects"]) {
		b.addMember(path.Join(transitionName, "effect", strconv.Itoa(index)))
	}
}

func triggerEvents(trigger anyMap) []string {
	events := arrayAny(trigger["events"])
	if len(events) == 0 && trigger["event"] != nil {
		events = []any{trigger["event"]}
	}
	names := make([]string, 0, len(events))
	for _, raw := range events {
		if name, err := eventNameValue(raw); err == nil {
			names = append(names, name)
		}
	}
	return names
}

func (r *runner) buildEntryPoint(entryPoint anyMap, ownerPath, sourceRoot, targetRoot string) (hsm.RedefinableElement, error) {
	name, err := requireString(entryPoint, "name")
	if err != nil {
		return nil, err
	}
	target, err := requireString(entryPoint, "target")
	if err != nil {
		return nil, err
	}
	resolvedTarget := resolveInitialTarget(target, ownerPath, sourceRoot, targetRoot)
	parts := []hsm.RedefinableElement{hsm.Target(buildPathExpression(target, resolvedTarget, sourceRoot, targetRoot))}
	effectRefs := arrayAny(entryPoint["effects"])
	if len(effectRefs) > 0 {
		ids, err := r.requireBehaviorIDs(effectRefs)
		if err != nil {
			return nil, err
		}
		parts = append(parts, hsm.Effect(r.effectCallback(ids, targetRoot)))
	}
	return hsm.EntryPoint(name, parts...), nil
}

func (r *runner) buildExitPoint(exitPoint anyMap, boundary string) (hsm.RedefinableElement, error) {
	name, err := requireString(exitPoint, "name")
	if err != nil {
		return nil, err
	}
	ids, err := r.requireBehaviorIDs(arrayAny(exitPoint["effects"]))
	if err != nil {
		return nil, err
	}
	parts := make([]hsm.RedefinableElement, 0, 1)
	if len(ids) > 0 {
		parts = append(parts, hsm.Effect(r.effectCallback(ids, boundary)))
	}
	r.exitPoints[path.Join(boundary, name)] = exitPointDef{boundary: boundary, name: name, ids: ids}
	return hsm.ExitPoint(name, parts...), nil
}

func (r *runner) buildModel(modelIR anyMap) (*hsm.FinalizedModel, error) {
	name, err := requireString(modelIR, "name")
	if err != nil {
		return nil, err
	}
	if model := r.models[name]; model != nil {
		return model, nil
	}
	effectiveModelIR := r.modelIRs[name]
	if effectiveModelIR == nil {
		effectiveModelIR = modelIR
	}
	var baseModel *hsm.FinalizedModel
	if _, ok := modelIR["redefines"]; ok {
		baseName, err := requireStringValue(modelIR["redefines"])
		if err != nil {
			return nil, fmt.Errorf("missing_submachine_model: %w", err)
		}
		baseIR := r.rawModelIRs[baseName]
		if baseIR == nil {
			return nil, fmt.Errorf("missing_submachine_model: %s", baseName)
		}
		baseModel, err = r.buildModel(baseIR)
		if err != nil {
			return nil, err
		}
	}
	parts := make([]hsm.RedefinableElement, 0)
	attrNames := make([]string, 0)
	for attrName, specAny := range object(modelIR["attributes"]) {
		attrNames = append(attrNames, attrName)
		spec := object(specAny)
		r.registerAttrSpec(attrName, spec)
		r.registerAttrSpec(path.Join("/"+name, attrName), spec)
		parts = append(parts, r.attributeBuilder(attrName, spec))
	}
	sort.Strings(attrNames)
	r.attrs[name] = attrNames
	r.scopedAttrs["/"+name] = attrNames
	for attrName, specAny := range object(effectiveModelIR["attributes"]) {
		spec := object(specAny)
		r.registerAttrSpec(attrName, spec)
		r.registerAttrSpec(path.Join("/"+name, attrName), spec)
	}
	for opName, refAny := range object(modelIR["operations"]) {
		behaviorID, err := r.requireBehaviorID(refAny)
		if err != nil {
			return nil, err
		}
		parts = append(parts, hsm.Operation(opName, r.operationCallback("", opName, behaviorID)))
	}
	if err := r.registerOperationBindings(name, effectiveModelIR); err != nil {
		return nil, err
	}
	for _, raw := range arrayAny(modelIR["entry_points"]) {
		entryPoint, err := r.buildEntryPoint(object(raw), "/"+name, "/"+name, "/"+name)
		if err != nil {
			return nil, err
		}
		parts = append(parts, entryPoint)
	}
	for _, raw := range arrayAny(modelIR["exit_points"]) {
		exitPoint, err := r.buildExitPoint(object(raw), "/"+name)
		if err != nil {
			return nil, err
		}
		parts = append(parts, exitPoint)
	}
	initial, ok := modelIR["initial"]
	if !ok && baseModel == nil {
		return nil, fmt.Errorf("missing initial")
	}
	if ok {
		initialPart, err := r.buildInitial(initial, "/"+name, "/"+name, "/"+name)
		if err != nil {
			return nil, err
		}
		parts = append(parts, initialPart)
	}
	for _, stateAny := range arrayAny(modelIR["states"]) {
		stateIR := object(stateAny)
		if stateIR == nil {
			return nil, fmt.Errorf("state must be an object")
		}
		part, err := r.buildState(stateIR, "/"+name, "/"+name, "/"+name, nil, nil)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	transitionOrdinal := 0
	for transitionIndex, transitionAny := range arrayAny(modelIR["transitions"]) {
		transitionIR := object(transitionAny)
		if rootTransitionRequiresExpansion(transitionIR) {
			expanded, err := rootTransitionExpansions(modelIR, "/"+name, transitionIR, transitionIndex)
			if err != nil {
				return nil, err
			}
			for _, expandedTransition := range expanded {
				transitionOrdinal++
				part, err := r.buildTransition(expandedTransition, "/"+name, false, "/"+name, "/"+name, transitionOrdinal)
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			}
			continue
		}
		transitionOrdinal++
		part, err := r.buildTransition(transitionIR, "/"+name, false, "/"+name, "/"+name, transitionOrdinal)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	exitPointPaths := make([]string, 0)
	for exitPointPath := range r.exitPoints {
		if exitPointPath == "/"+name || strings.HasPrefix(exitPointPath, "/"+name+"/") {
			exitPointPaths = append(exitPointPaths, exitPointPath)
		}
	}
	sort.Strings(exitPointPaths)
	modelRoot := "/" + name
	for _, exitPointPath := range exitPointPaths {
		def := r.exitPoints[exitPointPath]
		parts = append(parts, hsm.Transition(
			hsm.Source(buildPathExpression(exitPointPath, exitPointPath, modelRoot, modelRoot)),
			hsm.Target(buildPathExpression(def.boundary, def.boundary, modelRoot, modelRoot)),
			hsm.Effect(r.unhandledExitPointCallback(def.name)),
		))
	}
	attrSet := map[string]bool{}
	effectiveAttrNames := make([]string, 0)
	for attrName := range object(effectiveModelIR["attributes"]) {
		effectiveAttrNames = append(effectiveAttrNames, attrName)
	}
	if len(effectiveAttrNames) == 0 {
		effectiveAttrNames = attrNames
	}
	mergedAttrNames := make([]string, 0, len(effectiveAttrNames))
	for _, attrName := range effectiveAttrNames {
		if !attrSet[attrName] {
			attrSet[attrName] = true
			mergedAttrNames = append(mergedAttrNames, attrName)
		}
	}
	sort.Strings(mergedAttrNames)
	if len(mergedAttrNames) > 0 {
		r.attrs[name] = mergedAttrNames
		r.scopedAttrs["/"+name] = mergedAttrNames
	}
	var model hsm.FinalizedModel
	if baseModel != nil {
		args := make([]any, 0, len(parts)+1)
		args = append(args, name)
		for _, part := range parts {
			args = append(args, part)
		}
		model = hsm.Redefine(*baseModel, args...)
	} else {
		model = hsm.Define(name, parts...)
	}
	r.models[name] = &model
	r.bindModelTimerEventNames(&model)
	return &model, nil
}

func (r *runner) registerOperationBindings(modelName string, modelIR anyMap) error {
	for opName, refAny := range object(modelIR["operations"]) {
		behaviorID, err := r.requireBehaviorID(refAny)
		if err != nil {
			return err
		}
		operationPath := path.Join("/"+modelName, opName)
		r.operations[operationPath] = true
		r.operationBehaviors[operationPath] = behaviorID
	}
	return nil
}

func redefinedModelIRFrom(modelIRs map[string]anyMap, modelIR anyMap, seen map[string]bool) (anyMap, error) {
	name, err := requireString(modelIR, "name")
	if err != nil {
		return nil, err
	}
	if seen[name] {
		return nil, fmt.Errorf("submachine_model_cycle: recursive model redefine %s", name)
	}
	seen[name] = true
	defer delete(seen, name)
	baseName, err := requireStringValue(modelIR["redefines"])
	if err != nil {
		return nil, fmt.Errorf("missing_submachine_model: %w", err)
	}
	baseIR := modelIRs[baseName]
	if baseIR == nil {
		return nil, fmt.Errorf("missing_submachine_model: %s", baseName)
	}
	if _, ok := baseIR["redefines"]; ok {
		baseIR, err = redefinedModelIRFrom(modelIRs, baseIR, seen)
		if err != nil {
			return nil, err
		}
		modelIRs[baseName] = baseIR
	}
	merged := cloneObject(baseIR)
	merged["name"] = name
	delete(merged, "redefines")
	for _, field := range []string{"attributes", "operations"} {
		merged[field] = mergeObjects(object(merged[field]), object(modelIR[field]))
	}
	for _, field := range []string{"entry_points", "exit_points"} {
		merged[field] = mergeNamedArray(arrayAny(merged[field]), arrayAny(normalizeJSONValue(modelIR[field])))
	}
	overlayStates := arrayAny(normalizeJSONValue(modelIR["states"]))
	replacedStates := namedArrayNames(overlayStates)
	merged["states"] = mergeNamedArray(arrayAny(merged["states"]), overlayStates)
	merged["transitions"] = mergeTransitionArray(
		arrayAny(merged["transitions"]),
		arrayAny(normalizeJSONValue(modelIR["transitions"])),
		baseName,
		name,
		replacedStates,
	)
	for _, field := range []string{"observations"} {
		merged[field] = append(arrayAny(merged[field]), arrayAny(normalizeJSONValue(modelIR[field]))...)
	}
	if initial, ok := modelIR["initial"]; ok {
		merged["initial"] = normalizeJSONValue(initial)
	}
	return merged, nil
}

func cloneObject(value any) anyMap {
	cloned := object(normalizeJSONValue(value))
	if cloned == nil {
		return anyMap{}
	}
	return cloned
}

func mergeObjects(base, overlay anyMap) anyMap {
	merged := cloneObject(base)
	for key, value := range overlay {
		merged[key] = normalizeJSONValue(value)
	}
	return merged
}

func mergeNamedArray(base, overlay []any) []any {
	merged := make([]any, 0, len(base)+len(overlay))
	used := map[int]bool{}
	for _, baseItem := range base {
		baseName, _ := object(baseItem)["name"].(string)
		replacementIndex := -1
		if baseName != "" {
			for index, overlayItem := range overlay {
				overlayName, _ := object(overlayItem)["name"].(string)
				if !used[index] && overlayName == baseName {
					replacementIndex = index
					break
				}
			}
		}
		if replacementIndex >= 0 {
			merged = append(merged, normalizeJSONValue(overlay[replacementIndex]))
			used[replacementIndex] = true
			continue
		}
		merged = append(merged, normalizeJSONValue(baseItem))
	}
	for index, overlayItem := range overlay {
		if !used[index] {
			merged = append(merged, normalizeJSONValue(overlayItem))
		}
	}
	return merged
}

func namedArrayNames(values []any) map[string]bool {
	names := map[string]bool{}
	for _, value := range values {
		name, _ := object(value)["name"].(string)
		if name != "" {
			names[name] = true
		}
	}
	return names
}

func mergeTransitionArray(base, overlay []any, baseName, modelName string, replacedStates map[string]bool) []any {
	overlayIDs := map[string]bool{}
	for _, value := range overlay {
		id, _ := object(value)["id"].(string)
		if id != "" {
			overlayIDs[id] = true
		}
	}
	merged := make([]any, 0, len(base)+len(overlay))
	for _, value := range base {
		transition := object(value)
		id, _ := transition["id"].(string)
		if id != "" && overlayIDs[id] {
			continue
		}
		if transitionDependsOnReplacedState(transition, baseName, modelName, replacedStates) {
			continue
		}
		merged = append(merged, normalizeJSONValue(value))
	}
	for _, value := range overlay {
		merged = append(merged, normalizeJSONValue(value))
	}
	return merged
}

func transitionDependsOnReplacedState(transition anyMap, baseName, modelName string, replacedStates map[string]bool) bool {
	if len(replacedStates) == 0 {
		return false
	}
	if source, ok := transition["source"]; ok {
		sourceName, err := requireStringValue(source)
		if err == nil {
			top, ok := rootRelativeTop(sourceName, baseName, modelName)
			if ok && replacedStates[top] {
				return true
			}
		}
	}
	if target, ok := transition["target"]; ok {
		targetName, err := requireStringValue(target)
		if err == nil {
			top, descendant := rootRelativeTop(targetName, baseName, modelName)
			if descendant && replacedStates[top] && targetReferencesDescendant(targetName, baseName, modelName, top) {
				return true
			}
		}
	}
	return false
}

func rootRelativeTop(raw, baseName, modelName string) (string, bool) {
	clean := path.Clean(raw)
	if clean == "." || clean == "/" || clean == "" {
		return "", false
	}
	trimmed := strings.TrimPrefix(clean, "./")
	trimmed = strings.TrimPrefix(trimmed, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" || parts[0] == ".." {
		return "", false
	}
	if parts[0] == baseName || parts[0] == modelName {
		if len(parts) < 2 {
			return "", false
		}
		return parts[1], true
	}
	return parts[0], true
}

func targetReferencesDescendant(raw, baseName, modelName, top string) bool {
	clean := path.Clean(raw)
	trimmed := strings.TrimPrefix(clean, "./")
	trimmed = strings.TrimPrefix(trimmed, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return false
	}
	if parts[0] == baseName || parts[0] == modelName {
		return len(parts) > 2 && parts[1] == top
	}
	return len(parts) > 1 && parts[0] == top
}

func rootTransitionRequiresExpansion(transitionIR anyMap) bool {
	if transitionIR == nil {
		return false
	}
	return transitionIR["source"] == nil && transitionIR["target"] != nil && transitionTriggerKind(transitionIR) != "completion"
}

func rootTransitionExpansions(modelIR anyMap, rootPath string, transitionIR anyMap, transitionIndex int) ([]anyMap, error) {
	if err := validateRootTransitionExpansion(transitionIR); err != nil {
		return nil, err
	}
	sources := rootTransitionSourcePaths(modelIR, rootPath)
	if len(sources) == 0 {
		return nil, fmt.Errorf("unsupported_root_transition: source-less root transition requires at least one top-level state")
	}
	expanded := make([]anyMap, 0, len(sources))
	for sourceIndex, sourcePath := range sources {
		expanded = append(expanded, rootTransitionWithSource(transitionIR, sourcePath, transitionIndex, sourceIndex))
	}
	return expanded, nil
}

func validateRootTransitionExpansion(transitionIR anyMap) error {
	if transitionIR == nil {
		return fmt.Errorf("unsupported_root_transition: transition must be an object")
	}
	if transitionIR["source"] != nil {
		return nil
	}
	if transitionIR["target"] == nil {
		return nil
	}
	if transitionTriggerKind(transitionIR) == "completion" {
		return fmt.Errorf("unsupported_root_transition: source-less completion root transitions cannot be expanded")
	}
	trigger := object(transitionIR["trigger"])
	if trigger == nil {
		if _, ok := transitionIR["on"]; ok {
			trigger = anyMap{"kind": "on"}
		}
	}
	if trigger == nil {
		return fmt.Errorf("unsupported_root_transition: source-less root transition requires an explicit trigger")
	}
	switch kind, _ := trigger["kind"].(string); kind {
	case "on", "on_call", "on_set", "when", "after", "every", "at":
		return nil
	default:
		return fmt.Errorf("unsupported_root_transition: source-less root transition trigger %q cannot be expanded", kind)
	}
}

func rootTransitionSourcePaths(modelIR anyMap, rootPath string) []string {
	sources := make([]string, 0)
	for _, stateAny := range arrayAny(modelIR["states"]) {
		stateIR := object(stateAny)
		if stateIR == nil {
			continue
		}
		name, ok := stateIR["name"].(string)
		if !ok || name == "" {
			continue
		}
		sources = append(sources, path.Clean(rootPath+"/"+name))
	}
	return sources
}

func rootTransitionWithSource(transitionIR anyMap, sourcePath string, transitionIndex, sourceIndex int) anyMap {
	clone := make(anyMap, len(transitionIR)+1)
	for key, value := range transitionIR {
		clone[key] = value
	}
	clone["source"] = sourcePath
	if id, ok := clone["id"].(string); ok && id != "" {
		clone["id"] = fmt.Sprintf("%s__root_%d_%d", id, transitionIndex, sourceIndex)
	}
	return clone
}

func (r *runner) buildInitial(raw any, ownerPath, sourceRoot, targetRoot string) (hsm.RedefinableElement, error) {
	var target string
	var effects []any
	if s, ok := raw.(string); ok {
		target = s
	} else {
		m := object(raw)
		var err error
		target, err = requireString(m, "target")
		if err != nil {
			return nil, err
		}
		effects = arrayAny(m["effects"])
	}
	resolvedTarget := resolveInitialTarget(target, ownerPath, sourceRoot, targetRoot)
	parts := []hsm.RedefinableElement{hsm.Target(buildPathExpression(target, resolvedTarget, sourceRoot, targetRoot))}
	if len(effects) > 0 {
		ids := make([]string, 0, len(effects))
		for _, ref := range effects {
			behaviorID, err := r.requireBehaviorID(ref)
			if err != nil {
				return nil, err
			}
			ids = append(ids, behaviorID)
		}
		parts = append(parts, hsm.Effect(r.effectCallback(ids, targetRoot)))
	}
	return hsm.Initial(parts...), nil
}

func (r *runner) registerAttrSpec(name string, spec anyMap) {
	if name == "" {
		return
	}
	typ, _ := spec["type"].(string)
	if typ == "" {
		typ = inferAttrType(spec["default"])
	}
	r.attrSpecs[name] = attrSpec{typ: typ}
}

func (r *runner) buildState(stateIR anyMap, ownerPath, sourceRoot, targetRoot string, prependedTransitions, postpendedTransitions map[string][]anyMap) (hsm.RedefinableElement, error) {
	name, err := requireString(stateIR, "name")
	if err != nil {
		return nil, err
	}
	if strings.Contains(name, "/") {
		return nil, fmt.Errorf("invalid_name: state name %q", name)
	}
	statePath := path.Clean(ownerPath + "/" + name)
	parts := make([]hsm.RedefinableElement, 0)
	kindName, _ := stateIR["kind"].(string)
	if kindName == "final" {
		r.finalStates[statePath] = true
	}
	if kindName == "submachine" {
		r.submachineStates[statePath] = true
		if machineName, ok := stateIR["machine"].(string); ok {
			r.submachineModels[statePath] = machineName
		}
		if _, ok := stateIR["initial"]; ok {
			return nil, fmt.Errorf("invalid_submachine_initial: submachine state %q must not declare initial", statePath)
		}
		if len(arrayAny(stateIR["states"])) > 0 {
			return nil, fmt.Errorf("invalid_submachine_contents: submachine state %q must not declare child states", statePath)
		}
	} else if _, ok := stateIR["machine"]; ok {
		return nil, fmt.Errorf("invalid_submachine_machine: non-submachine state %q must not declare machine", statePath)
	}
	if initial, ok := stateIR["initial"]; ok {
		part, err := r.buildInitial(initial, statePath, sourceRoot, targetRoot)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	for _, field := range []string{"entry", "exit", "activity"} {
		refs := arrayAny(stateIR[field])
		if len(refs) == 0 {
			continue
		}
		ids := make([]string, 0, len(refs))
		for _, ref := range refs {
			behaviorID, err := r.requireBehaviorID(ref)
			if err != nil {
				return nil, err
			}
			ids = append(ids, behaviorID)
		}
		switch field {
		case "entry":
			parts = append(parts, hsm.Entry(r.entryCallback(ids, targetRoot, statePath)))
		case "exit":
			parts = append(parts, hsm.Exit(r.exitCallback(ids, targetRoot, statePath)))
		case "activity":
			parts = append(parts, hsm.Activity(r.activityCallback(ids, targetRoot, statePath)))
		}
	}
	if rawDefer, hasDefer := stateIR["defer"]; hasDefer {
		deferEvents := arrayAny(rawDefer)
		if len(deferEvents) == 0 {
			parts = append(parts, hsm.Defer([]hsm.Event{}...))
		}
		for _, event := range deferEvents {
			name, err := eventNameValue(event)
			if err != nil {
				return nil, err
			}
			if r.deferEvents[statePath] == nil {
				r.deferEvents[statePath] = map[string]bool{}
			}
			r.deferEvents[statePath][name] = true
			parts = append(parts, hsm.Defer(name))
		}
	}
	transitionOwner := statePath
	bareTargets := false
	var submachineModel hsm.Model
	if kindName == "choice" || kindName == "shallow_history" || kindName == "deep_history" {
		transitionOwner = ownerPath
		bareTargets = true
	}
	if kindName == "submachine" {
		childParts, err := r.buildSubmachineParts(stateIR, statePath)
		if err != nil {
			return nil, err
		}
		submachineModel = hsm.InlineModel(childParts...)
	}
	if kindName == "choice" {
		transitions := arrayAny(stateIR["transitions"])
		if len(transitions) == 0 {
			return nil, fmt.Errorf("choice_missing_fallback: choice %q requires a fallback transition", statePath)
		}
		last := object(transitions[len(transitions)-1])
		if last == nil || last["guard"] != nil {
			return nil, fmt.Errorf("choice_missing_fallback: choice %q requires a fallback transition", statePath)
		}
	}
	for _, child := range arrayAny(stateIR["states"]) {
		part, err := r.buildState(object(child), statePath, sourceRoot, targetRoot, prependedTransitions, postpendedTransitions)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	transitionOrdinal := 0
	for _, transition := range prependedTransitions[statePath] {
		transitionOrdinal++
		part, err := r.buildTransition(transition, transitionOwner, bareTargets, sourceRoot, targetRoot, transitionOrdinal)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	for _, transition := range arrayAny(stateIR["transitions"]) {
		transitionOrdinal++
		part, err := r.buildTransition(object(transition), transitionOwner, bareTargets, sourceRoot, targetRoot, transitionOrdinal)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	for _, transition := range postpendedTransitions[statePath] {
		transitionOrdinal++
		part, err := r.buildTransition(transition, transitionOwner, bareTargets, sourceRoot, targetRoot, transitionOrdinal)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	switch kindName {
	case "", "state":
		return hsm.State(name, parts...), nil
	case "final":
		return hsm.Final(name), nil
	case "choice":
		return hsm.Choice(name, parts...), nil
	case "shallow_history":
		return hsm.ShallowHistory(name, parts...), nil
	case "deep_history":
		return hsm.DeepHistory(name, parts...), nil
	case "submachine":
		return hsm.SubmachineState(name, submachineModel, parts...), nil
	default:
		return nil, fmt.Errorf("unsupported state kind %q", kindName)
	}
}

func (r *runner) buildSubmachineParts(stateIR anyMap, statePath string) ([]hsm.RedefinableElement, error) {
	machineName, err := requireString(stateIR, "machine")
	if err != nil {
		return nil, fmt.Errorf("missing_submachine_machine: %w", err)
	}
	childModel := r.modelIRs[machineName]
	if childModel == nil {
		return nil, fmt.Errorf("missing_submachine_machine: %q", machineName)
	}
	for _, active := range r.submachineStack {
		if active == machineName {
			return nil, fmt.Errorf("submachine_model_cycle: %s", machineName)
		}
	}
	r.submachineStack = append(r.submachineStack, machineName)
	defer func() {
		r.submachineStack = r.submachineStack[:len(r.submachineStack)-1]
	}()
	childRoot := "/" + machineName
	r.collectSubmachineStates(childModel, statePath, childRoot, statePath)
	if err := r.validateEntryPoints(childModel, childRoot, statePath); err != nil {
		return nil, err
	}
	if err := r.validateExitPoints(childModel, childRoot, statePath); err != nil {
		return nil, err
	}
	parts := make([]hsm.RedefinableElement, 0)
	childAttrNames := make([]string, 0)
	for attrName, specAny := range object(childModel["attributes"]) {
		childAttrNames = append(childAttrNames, attrName)
		spec := object(specAny)
		qualifiedAttr := path.Join(rootPath(statePath), attrName)
		r.registerAttrSpec(attrName, spec)
		r.registerAttrSpec(qualifiedAttr, spec)
		parts = append(parts, r.attributeBuilder(attrName, spec))
	}
	sort.Strings(childAttrNames)
	r.scopedAttrs[statePath] = childAttrNames
	for opName, refAny := range object(childModel["operations"]) {
		behaviorID, err := r.requireBehaviorID(refAny)
		if err != nil {
			return nil, err
		}
		operationPath := path.Join(rootPath(statePath), opName)
		parts = append(parts, hsm.Operation(opName, r.operationCallback(rootPath(statePath), opName, behaviorID)))
		r.operations[operationPath] = true
		r.operationBehaviors[operationPath] = behaviorID
	}
	for _, raw := range arrayAny(childModel["entry_points"]) {
		entryPoint, err := r.buildEntryPoint(object(raw), statePath, childRoot, statePath)
		if err != nil {
			return nil, err
		}
		parts = append(parts, entryPoint)
	}
	for _, raw := range arrayAny(childModel["exit_points"]) {
		exitPoint, err := r.buildExitPoint(object(raw), statePath)
		if err != nil {
			return nil, err
		}
		parts = append(parts, exitPoint)
	}
	initial, ok := childModel["initial"]
	if !ok {
		return nil, fmt.Errorf("missing initial")
	}
	initialPart, err := r.buildInitial(initial, statePath, childRoot, statePath)
	if err != nil {
		return nil, err
	}
	parts = append(parts, initialPart)
	buckets, err := partitionSubmachineTransitions(arrayAny(childModel["transitions"]), statePath, childRoot, true)
	if err != nil {
		return nil, err
	}
	for _, child := range arrayAny(childModel["states"]) {
		part, err := r.buildState(object(child), statePath, childRoot, statePath, buckets.prepended, buckets.postpended)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	transitionOrdinal := 0
	for _, transition := range buckets.root {
		transitionOrdinal++
		part, err := r.buildTransition(transition, statePath, false, childRoot, statePath, transitionOrdinal)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func (r *runner) collectSubmachineStates(modelIR anyMap, ownerPath, sourceRoot, targetRoot string) {
	r.collectSubmachineStatesSeen(modelIR, ownerPath, sourceRoot, targetRoot, map[string]bool{})
}

func (r *runner) collectSubmachineStatesSeen(modelIR anyMap, ownerPath, sourceRoot, targetRoot string, seen map[string]bool) {
	for _, stateAny := range arrayAny(modelIR["states"]) {
		stateIR := object(stateAny)
		name, _ := stateIR["name"].(string)
		if name == "" {
			continue
		}
		statePath := path.Join(ownerPath, name)
		kindName, _ := stateIR["kind"].(string)
		if kindName == "submachine" {
			r.submachineStates[statePath] = true
			if machineName, ok := stateIR["machine"].(string); ok {
				r.submachineModels[statePath] = machineName
			}
			machineName, _ := stateIR["machine"].(string)
			childModel := r.modelIRs[machineName]
			if childModel != nil && !seen[machineName] {
				r.collectSubmachineExitPoints(childModel, statePath)
				seen[machineName] = true
				r.collectSubmachineStatesSeen(childModel, statePath, "/"+machineName, statePath, seen)
				delete(seen, machineName)
			}
			continue
		}
		r.collectSubmachineStatesSeen(stateIR, statePath, sourceRoot, targetRoot, seen)
	}
}

func (r *runner) collectSubmachineExitPoints(modelIR anyMap, boundary string) {
	for _, raw := range arrayAny(modelIR["exit_points"]) {
		exitPoint := object(raw)
		name, _ := exitPoint["name"].(string)
		if name == "" {
			continue
		}
		if _, exists := r.exitPoints[path.Join(boundary, name)]; !exists {
			r.exitPoints[path.Join(boundary, name)] = exitPointDef{boundary: boundary, name: name}
		}
	}
}

func (r *runner) isSubmachineInternalPath(value string) bool {
	clean := path.Clean(value)
	for boundary := range r.submachineStates {
		if clean != boundary && strings.HasPrefix(clean, boundary+"/") {
			return true
		}
	}
	return false
}

func (r *runner) validateEntryPoints(modelIR anyMap, sourceRoot, targetRoot string) error {
	seen := map[string]bool{}
	entryPointNames := map[string]bool{}
	stateKinds := map[string]string{}
	exitPoints := map[string]bool{}
	for _, stateAny := range arrayAny(modelIR["states"]) {
		stateIR := object(stateAny)
		if name, ok := stateIR["name"].(string); ok {
			kindName, _ := stateIR["kind"].(string)
			stateKinds[name] = kindName
		}
	}
	for _, raw := range arrayAny(modelIR["exit_points"]) {
		exitPoint := object(raw)
		if name, ok := exitPoint["name"].(string); ok {
			exitPoints[name] = true
		}
	}
	for _, raw := range arrayAny(modelIR["entry_points"]) {
		entryPoint := object(raw)
		if name, ok := entryPoint["name"].(string); ok {
			entryPointNames[name] = true
		}
	}
	for _, raw := range arrayAny(modelIR["entry_points"]) {
		entryPoint := object(raw)
		name, err := requireString(entryPoint, "name")
		if err != nil {
			return err
		}
		if strings.Contains(name, "/") {
			return fmt.Errorf("invalid_name: entry point %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate_entry_point: %s", name)
		}
		seen[name] = true
		if kindName, ok := stateKinds[name]; ok && kindName != "final" && kindName != "submachine" {
			return fmt.Errorf("connection_point_name_collision: %s", name)
		}
		if effects, ok := entryPoint["effects"]; ok && len(arrayAny(effects)) == 0 {
			return fmt.Errorf("empty_behavior_array: entry point %s", name)
		}
		target, err := requireString(entryPoint, "target")
		if err != nil {
			return fmt.Errorf("missing_target: %w", err)
		}
		if strings.HasPrefix(target, "/") {
			clean := path.Clean(target)
			if clean != sourceRoot && !strings.HasPrefix(clean, sourceRoot+"/") {
				return fmt.Errorf("invalid_entry_point_target: %s", clean)
			}
		}
		if entryPointNames[target] && stateKinds[target] == "" {
			return fmt.Errorf("invalid_entry_point_target: %s", target)
		}
		if exitPoints[target] {
			return fmt.Errorf("invalid_entry_point_target_kind: %s", target)
		}
		resolved := resolvePathInScope(target, targetRoot, true, sourceRoot, targetRoot)
		if !r.pathExistsInModel(modelIR, sourceRoot, targetRoot, resolved) {
			return fmt.Errorf("missing_target: %s", resolved)
		}
		for _, ref := range arrayAny(entryPoint["effects"]) {
			if _, err := r.requireBehaviorID(ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) validateExitPoints(modelIR anyMap, sourceRoot, targetRoot string) error {
	seen := map[string]bool{}
	stateNames := map[string]bool{}
	for _, stateAny := range arrayAny(modelIR["states"]) {
		if name, ok := object(stateAny)["name"].(string); ok {
			stateNames[name] = true
		}
	}
	for _, raw := range arrayAny(modelIR["exit_points"]) {
		exitPoint := object(raw)
		name, err := requireString(exitPoint, "name")
		if err != nil {
			return err
		}
		if strings.Contains(name, "/") {
			return fmt.Errorf("invalid_name: exit point %q", name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate_exit_point: %s", name)
		}
		seen[name] = true
		if stateNames[name] {
			return fmt.Errorf("connection_point_name_collision: %s", name)
		}
		if effects, ok := exitPoint["effects"]; ok && len(arrayAny(effects)) == 0 {
			return fmt.Errorf("empty_behavior_array: exit point %s", name)
		}
		for _, ref := range arrayAny(exitPoint["effects"]) {
			if _, err := r.requireBehaviorID(ref); err != nil {
				return err
			}
		}
	}
	_ = sourceRoot
	return nil
}

func (r *runner) pathExistsInModel(modelIR anyMap, sourceRoot, targetRoot, want string) bool {
	var walk func([]any, string) bool
	walk = func(states []any, ownerPath string) bool {
		for _, stateAny := range states {
			stateIR := object(stateAny)
			name, _ := stateIR["name"].(string)
			if name == "" {
				continue
			}
			statePath := path.Join(ownerPath, name)
			if statePath == want {
				return true
			}
			if walk(arrayAny(stateIR["states"]), statePath) {
				return true
			}
			if stateIR["kind"] == "submachine" {
				machineName, _ := stateIR["machine"].(string)
				child := r.modelIRs[machineName]
				if child != nil {
					childRoot := "/" + machineName
					childTarget := resolvePathInScope(strings.TrimPrefix(want, statePath+"/"), statePath, true, childRoot, statePath)
					if strings.HasPrefix(want, statePath+"/") && r.pathExistsInModel(child, childRoot, statePath, childTarget) {
						return true
					}
				}
			}
		}
		return false
	}
	_ = sourceRoot
	return walk(arrayAny(modelIR["states"]), targetRoot)
}

func (r *runner) buildTransition(transitionIR anyMap, ownerPath string, bareTargets bool, sourceRoot, targetRoot string, transitionOrdinal int) (hsm.RedefinableElement, error) {
	return r.buildTransitionExpanded(transitionIR, ownerPath, bareTargets, sourceRoot, targetRoot, transitionOrdinal)
}

func (r *runner) buildTransitionExpanded(transitionIR anyMap, ownerPath string, bareTargets bool, sourceRoot, targetRoot string, transitionOrdinal int) (hsm.RedefinableElement, error) {
	if transitionIR == nil {
		return nil, fmt.Errorf("transition must be an object")
	}
	parts := make([]hsm.RedefinableElement, 0)
	hasKindOverride := false
	explicitTransitionKind := ""
	if kindName, ok := transitionIR["kind"].(string); ok {
		hasKindOverride = true
		explicitTransitionKind = kindName
		switch kindName {
		case "internal", "local", "external", "self":
			parts = append(parts, transitionKindOverride(transitionKind(kindName)))
		default:
			return nil, fmt.Errorf("unsupported transition kind %q", kindName)
		}
	}
	var sourcePath string
	var sourcePart hsm.RedefinableElement
	if rawSource, ok := transitionIR["source"]; ok {
		source, err := requireStringValue(rawSource)
		if err != nil {
			return nil, err
		}
		sourcePath = resolvePathInScope(source, ownerPath, bareTargets, sourceRoot, targetRoot)
		if sourceRoot == targetRoot && r.isSubmachineInternalPath(sourcePath) {
			return nil, fmt.Errorf("invalid_submachine_internal_source: %s", sourcePath)
		}
		sourcePart = hsm.Source(buildPathExpression(source, sourcePath, sourceRoot, targetRoot))
	}
	trigger := object(transitionIR["trigger"])
	if trigger == nil {
		if on, ok := transitionIR["on"]; ok {
			trigger = anyMap{"kind": "on", "event": on}
		}
	}
	var whenGuard func(context.Context, *confInstance, hsm.Event) bool
	var exitPointName string
	var exitPointBoundary string
	var exitPointPart hsm.RedefinableElement
	var completionGuard func(context.Context, *confInstance, hsm.Event) bool
	if trigger != nil {
		var part hsm.RedefinableElement
		var err error
		if trigger["kind"] == "when" {
			part, _, whenGuard, err = r.buildWhenTrigger(trigger, ownerPath, targetRoot)
		} else if trigger["kind"] == "exit_point" {
			exitPointName, err = r.exitPointName(trigger)
			if err == nil {
				exitPointBoundary = sourcePath
				if exitPointBoundary == "" {
					exitPointBoundary = ownerPath
				}
				err = r.validateExitPointHandler(exitPointBoundary, exitPointName)
			}
			if err == nil {
				_, err = r.resolveExitPoint(exitPointBoundary, exitPointName)
			}
			if err == nil {
				exitPointPart = hsm.ExitPoint(exitPointName)
			}
		} else {
			part, err = r.buildTrigger(trigger, targetRoot)
			if err == nil && trigger["kind"] == "completion" {
				completionGuard = func(_ context.Context, _ *confInstance, event hsm.Event) bool {
					return r.finalStates[event.Source]
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if part != nil {
			parts = append(parts, part)
		}
	}
	if sourcePart != nil {
		parts = append(parts, sourcePart)
	}
	if exitPointPart != nil {
		parts = append(parts, exitPointPart)
	}
	isTimerTrigger := trigger != nil && (trigger["kind"] == "after" || trigger["kind"] == "every" || trigger["kind"] == "at")
	timerEventName := ""
	if isTimerTrigger {
		kindName, _ := trigger["kind"].(string)
		timerEventName = r.timerEventNameForIR(ownerPath, kindName, transitionOrdinal)
	}
	hasGuardPart := false
	if guardRef, ok := transitionIR["guard"]; ok {
		behaviorID, err := r.requireBehaviorID(guardRef)
		if err != nil {
			return nil, err
		}
		hasGuardPart = true
		if isTimerTrigger {
			parts = append(parts, hsm.Guard(r.timerFiredGuard(behaviorID, true, targetRoot, timerEventName)))
		} else if whenGuard != nil {
			userGuard := r.guardCallback(behaviorID, targetRoot)
			parts = append(parts, hsm.Guard(func(ctx context.Context, sm *confInstance, event hsm.Event) bool {
				return whenGuard(ctx, sm, event) && userGuard(ctx, sm, event)
			}))
		} else if completionGuard != nil {
			userGuard := r.guardCallback(behaviorID, targetRoot)
			parts = append(parts, hsm.Guard(func(ctx context.Context, sm *confInstance, event hsm.Event) bool {
				return completionGuard(ctx, sm, event) && userGuard(ctx, sm, event)
			}))
		} else {
			parts = append(parts, hsm.Guard(r.guardCallback(behaviorID, targetRoot)))
		}
	} else if isTimerTrigger {
		hasGuardPart = true
		parts = append(parts, hsm.Guard(r.timerFiredGuard("", false, targetRoot, timerEventName)))
	} else if whenGuard != nil {
		hasGuardPart = true
		parts = append(parts, hsm.Guard(whenGuard))
	} else if completionGuard != nil {
		hasGuardPart = true
		parts = append(parts, hsm.Guard(completionGuard))
	}
	var resolvedTarget string
	entryPointName, hasEntryPoint := transitionIR["entry_point"].(string)
	if hasEntryPoint && strings.Contains(entryPointName, "/") {
		return nil, fmt.Errorf("invalid_name: entry point selector %q", entryPointName)
	}
	if rawTarget, ok := transitionIR["target"]; ok {
		target, err := requireStringValue(rawTarget)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(target, ".entry/") || strings.HasPrefix(target, ".exit/") {
			return nil, fmt.Errorf("invalid_entry_point_internal_target: %s", target)
		}
		if r.isInternalEntryPointTarget(sourceRoot, target) {
			return nil, fmt.Errorf("invalid_entry_point_internal_target: %s", target)
		}
		resolvedTarget = resolveTransitionTarget(target, ownerPath, sourcePath, bareTargets, sourceRoot, targetRoot)
		if hasEntryPoint {
			entryPointBoundary := resolvedTarget
			source := sourcePath
			if source == "" {
				source = ownerPath
			}
			if !hasKindOverride && entryPointBoundary == source {
				parts = append(parts, transitionKindOverride(hsm.ExternalKind))
			}
		}
		if !hasEntryPoint && sourceRoot == targetRoot && r.isSubmachineInternalPath(resolvedTarget) {
			return nil, fmt.Errorf("invalid_submachine_internal_target: %s", resolvedTarget)
		}
		if def, ok := r.exitPoints[resolvedTarget]; ok {
			if def.boundary != targetRoot {
				return nil, fmt.Errorf("invalid_submachine_internal_target: %s", resolvedTarget)
			}
		}
		parts = append(parts, hsm.Target(buildPathExpression(target, resolvedTarget, sourceRoot, targetRoot)))
		if hasEntryPoint {
			parts = append(parts, hsm.EntryPoint(entryPointName))
		}
	} else if hasEntryPoint {
		return nil, fmt.Errorf("invalid_entry_point_usage: entry point %q requires target", entryPointName)
	}
	if resolvedTarget != "" && transitionIR["guard"] == nil {
		source := sourcePath
		if source == "" {
			source = ownerPath
		}
		eventNames, err := r.transitionEventNames(transitionIR, targetRoot)
		if err != nil {
			return nil, err
		}
		for _, eventName := range eventNames {
			if r.activityExitEvents[source] == nil {
				r.activityExitEvents[source] = map[string]bool{}
			}
			r.activityExitEvents[source][eventName] = true
		}
	}
	effectRefs := arrayAny(transitionIR["effects"])
	if len(effectRefs) > 0 {
		ids, err := r.requireBehaviorIDs(effectRefs)
		if err != nil {
			return nil, err
		}
		parts = append(parts, hsm.Effect(r.effectCallback(ids, targetRoot)))
	}
	if len(effectRefs) == 0 && explicitTransitionKind == "internal" && hasGuardPart {
		parts = append(parts, hsm.Effect(func(context.Context, *confInstance, hsm.Event) {}))
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("transition must contain at least one part")
	}
	if id, ok := transitionIR["id"].(string); ok && id != "" {
		return hsm.Transition(id, parts...), nil
	}
	return hsm.Transition(parts[0], parts[1:]...), nil
}

func (r *runner) isInternalEntryPointTarget(sourceRoot, target string) bool {
	modelIR := r.modelIRs[rootName(sourceRoot)]
	if modelIR == nil || target == "" {
		return false
	}
	cleanTarget := path.Clean(target)
	for _, raw := range arrayAny(modelIR["entry_points"]) {
		entryPoint := object(raw)
		name, _ := entryPoint["name"].(string)
		if name == "" {
			continue
		}
		if cleanTarget == name || cleanTarget == path.Join(sourceRoot, name) {
			return true
		}
	}
	return false
}

func (r *runner) entryCallback(ids []string, scope, statePath string) func(context.Context, *confInstance, hsm.Event) {
	return func(ctx context.Context, sm *confInstance, event hsm.Event) {
		ctx = withBehaviorScope(ctx, scope)
		ctx = withBehaviorState(ctx, statePath)
		for _, id := range ids {
			if _, err := r.executeBehavior(ctx, sm, event, id, "entry"); err != nil {
				r.recordError(err)
				panic(err)
			}
		}
	}
}

func (r *runner) exitCallback(ids []string, scope, statePath string) func(context.Context, *confInstance, hsm.Event) {
	return func(ctx context.Context, sm *confInstance, event hsm.Event) {
		ctx = withBehaviorScope(ctx, scope)
		ctx = withBehaviorState(ctx, statePath)
		for _, id := range ids {
			if _, err := r.executeBehavior(ctx, sm, event, id, "exit"); err != nil {
				r.recordError(err)
				panic(err)
			}
		}
	}
}

func (r *runner) activityCallback(ids []string, scope, statePath string) func(context.Context, *confInstance, hsm.Event) {
	return func(ctx context.Context, sm *confInstance, event hsm.Event) {
		ctx = withBehaviorScope(ctx, scope)
		ctx = withBehaviorState(ctx, statePath)
		recorder := &activityCancelRecorder{cancelled: map[string]bool{}}
		resultCh := make(chan error, len(ids))
		startedCount := 0
		for _, id := range ids {
			id := id
			started := make(chan struct{})
			activityCtx := context.WithValue(context.WithValue(ctx, activityStartedKey{}, started), activityCancelRecorderKey{}, recorder)
			go func() {
				if _, err := r.executeBehavior(activityCtx, sm, event, id, "activity"); err != nil {
					if errors.Is(err, errBehaviorCancelled) {
						resultCh <- nil
						return
					}
					r.recordError(err)
					resultCh <- err
					return
				}
				if activityCtx.Err() == nil && id == "activity_done" {
					r.trace = append(r.trace, anyMap{"type": "activity_done", "behavior": id})
				}
				resultCh <- nil
			}()
			select {
			case <-started:
				startedCount++
			case <-ctx.Done():
				goto waitForCancellation
			case err := <-resultCh:
				if err != nil {
					return
				}
			}
		}
	waitForCancellation:
		for i := 0; i < startedCount; i++ {
			if err := <-resultCh; err != nil {
				return
			}
		}
		for _, id := range ids {
			if recorder.isCancelled(id) {
				r.appendActivityCancel(id)
			}
		}
	}
}

func (r *runner) effectCallback(ids []string, scope string) func(context.Context, *confInstance, hsm.Event) {
	return func(ctx context.Context, sm *confInstance, event hsm.Event) {
		ctx = withBehaviorScope(ctx, scope)
		for _, id := range ids {
			if _, err := r.executeBehavior(ctx, sm, event, id, "effect"); err != nil {
				r.recordError(err)
				panic(err)
			}
		}
	}
}

func (r *runner) guardCallback(id string, scope string) func(context.Context, *confInstance, hsm.Event) bool {
	return func(ctx context.Context, sm *confInstance, event hsm.Event) bool {
		ctx = withBehaviorScope(ctx, scope)
		value, err := r.executeBehavior(ctx, sm, event, id, "guard")
		if err != nil {
			r.recordError(err)
			panic(err)
		}
		return truthy(value)
	}
}

func (r *runner) unhandledExitPointCallback(name string) func(context.Context, *confInstance, hsm.Event) {
	return func(_ context.Context, _ *confInstance, event hsm.Event) {
		err := conformanceError{code: "unhandled_exit_point", message: name}
		r.trace = append(r.trace, anyMap{"type": "error", "code": err.code})
		r.recordError(err)
		panic(err)
	}
}

func withBehaviorScope(ctx context.Context, scope string) context.Context {
	if scope == "" {
		return ctx
	}
	return context.WithValue(ctx, behaviorScopeKey{}, scope)
}

func withBehaviorState(ctx context.Context, state string) context.Context {
	if state == "" {
		return ctx
	}
	return context.WithValue(ctx, behaviorStateKey{}, state)
}

func behaviorState(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	state, _ := ctx.Value(behaviorStateKey{}).(string)
	return state
}

func behaviorScope(ctx context.Context, fallback string) string {
	if ctx != nil {
		if scope, ok := ctx.Value(behaviorScopeKey{}).(string); ok && scope != "" {
			return scope
		}
	}
	return fallback
}

func (r *runner) scopedAttrName(ctx context.Context, name string) string {
	if name == "" || path.IsAbs(name) {
		return name
	}
	return r.attrNameInScope(behaviorScope(ctx, ""), name)
}

func (r *runner) attrNameInScope(scope, name string) string {
	if name == "" || path.IsAbs(name) {
		return name
	}
	return name
}

func (r *runner) operationCallback(scopePath, opName, behaviorID string) func(context.Context, *confInstance, ...any) (any, error) {
	return func(ctx context.Context, sm *confInstance, args ...any) (any, error) {
		if r.callTransitionFailed() {
			return nil, r.lastError
		}
		scope := scopePath
		if scope == "" && sm != nil {
			scope = hsm.QualifiedName(sm)
			if scope == "" {
				scope = rootPath(sm.State())
			}
		}
		ctx = withBehaviorScope(ctx, scope)
		eventName := opName
		if scope != "" {
			eventName = path.Join(scope, opName)
		}
		event := hsm.Event{Name: eventName, Kind: hsm.CallEventKind, Data: hsm.CallData{Name: opName, Args: args}}
		return r.executeBehavior(ctx, sm, event, behaviorID, "operation")
	}
}

func (r *runner) callTransitionFailed() bool {
	if len(r.callErrorBaselines) == 0 || r.lastError == nil {
		return false
	}
	return r.lastError != r.callErrorBaselines[len(r.callErrorBaselines)-1]
}

func (r *runner) call(ctx context.Context, sm *confInstance, operation string, args ...any) (any, error) {
	r.callErrorBaselines = append(r.callErrorBaselines, r.lastError)
	defer func() {
		r.callErrorBaselines = r.callErrorBaselines[:len(r.callErrorBaselines)-1]
	}()
	runtimeOperation := r.operationNameInScope(ctx, sm, operation)
	if r.inRuntimeProcessing(ctx, sm) || behaviorScope(ctx, "") != "" || behaviorState(ctx) != "" {
		return r.callInRuntimeProcessing(ctx, sm, runtimeOperation, args...)
	}
	return hsm.Call(ctx, sm, runtimeOperation, args...)
}

func (r *runner) callInRuntimeProcessing(ctx context.Context, sm *confInstance, runtimeOperation string, args ...any) (any, error) {
	if runtimeOperation == "" {
		return nil, hsm.ErrInvalidOperation
	}
	operationPath := runtimeOperation
	if !path.IsAbs(operationPath) {
		if sm == nil {
			return nil, hsm.ErrMissingHSM
		}
		operationPath = path.Join(rootPath(sm.State()), runtimeOperation)
	}
	behaviorID := r.operationBehaviors[operationPath]
	if behaviorID == "" {
		return nil, hsm.ErrMissingOperation
	}
	opName := path.Base(operationPath)
	event := hsm.Event{
		Name: operationPath,
		Kind: hsm.CallEventKind,
		Data: hsm.CallData{Name: opName, Args: args},
	}
	result, err := r.executeBehavior(withBehaviorScope(ctx, path.Dir(operationPath)), sm, event, behaviorID, "operation")
	if err != nil {
		return result, err
	}
	callEvent := hsm.Event{
		Name:   operationPath,
		Kind:   hsm.CallEventKind,
		Source: operationPath,
		Data:   hsm.CallData{Name: operationPath, Args: args},
	}
	if r.instanceQueues[r.instanceIDFor(sm)] != "" {
		wait := sm.Wait()
		done := make(chan error)
		go func() {
			defer close(done)
			select {
			case <-wait:
			case <-ctx.Done():
				return
			}
			select {
			case <-hsm.Dispatch(ctx, sm, callEvent):
			case <-ctx.Done():
				r.recordError(conformanceError{code: "runtime_wait_cancelled", message: "operation call " + operationPath})
			}
		}()
		r.addPendingWork(hsm.Completion(done))
		return result, nil
	}
	r.addPendingWork(hsm.Dispatch(ctx, sm, callEvent))
	return result, nil
}

func (r *runner) operationNameInScope(ctx context.Context, sm *confInstance, name string) string {
	if name == "" || path.IsAbs(name) {
		return name
	}
	if scope := behaviorScope(ctx, ""); scope != "" {
		if operation := r.operationNameInScopePath(scope, name); operation != "" {
			return operation
		}
	}
	if state := behaviorState(ctx); state != "" {
		if operation := r.operationNameInScopePath(state, name); operation != "" {
			return operation
		}
	}
	if sm != nil {
		state := sm.State()
		if operation := r.operationNameInScopePath(state, name); operation != "" {
			return operation
		}
	}
	return name
}

func (r *runner) operationNameInScopePath(scope, name string) string {
	if name == "" || path.IsAbs(name) {
		return name
	}
	for current := path.Clean(scope); current != "" && current != "." && current != "/"; {
		candidate := path.Join(current, name)
		if r.operations[candidate] {
			return candidate
		}
		next := path.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return ""
}

func (r *runner) inRuntimeProcessing(ctx context.Context, sm *confInstance) bool {
	_, ok := hsm.FromContext(ctx)
	return ok
}

func (r *runner) buildTrigger(trigger anyMap, scope string) (hsm.RedefinableElement, error) {
	kindName, _ := trigger["kind"].(string)
	switch kindName {
	case "on":
		if err := validateOnlyKeys(trigger, "kind", "event", "events"); err != nil {
			return nil, fmt.Errorf("extraneous_trigger_operand: %w", err)
		}
		events := []any{}
		if rawEvents, hasEvents := trigger["events"]; hasEvents {
			events = arrayAny(rawEvents)
		} else {
			events = []any{trigger["event"]}
		}
		onEvents := make([]hsm.Event, 0, len(events))
		for _, event := range events {
			name, err := eventNameValue(event)
			if err != nil {
				return nil, err
			}
			onEvents = append(onEvents, eventForOnName(name))
		}
		return hsm.On(onEvents...), nil
	case "on_set":
		if err := validateOnlyKeys(trigger, "kind", "attribute"); err != nil {
			return nil, fmt.Errorf("extraneous_trigger_operand: %w", err)
		}
		name, err := requireString(trigger, "attribute")
		if err != nil {
			return nil, err
		}
		name = r.attrNameInScope(scope, name)
		r.traceSetAttrs[name] = true
		return hsm.OnSet(localBuilderName(name)), nil
	case "on_call":
		if err := validateOnlyKeys(trigger, "kind", "operation"); err != nil {
			return nil, fmt.Errorf("extraneous_trigger_operand: %w", err)
		}
		name, err := requireString(trigger, "operation")
		if err != nil {
			return nil, err
		}
		if path.IsAbs(name) {
			return hsm.On(hsm.Event{Name: name, Kind: hsm.CallEventKind}), nil
		}
		return hsm.OnCall(name), nil
	case "completion":
		if err := validateOnlyKeys(trigger, "kind"); err != nil {
			return nil, fmt.Errorf("extraneous_trigger_operand: %w", err)
		}
		return hsm.On(hsm.FinalEvent), nil
	case "when":
		return nil, fmt.Errorf("when trigger should be built by buildWhenTrigger")
	case "after", "every", "at":
		switch kindName {
		case "after", "every":
			if err := validateOnlyKeys(trigger, "kind", "duration_ms", "attribute", "behavior"); err != nil {
				return nil, fmt.Errorf("extraneous_trigger_operand: %w", err)
			}
		case "at":
			if err := validateOnlyKeys(trigger, "kind", "time_ms", "attribute", "behavior"); err != nil {
				return nil, fmt.Errorf("extraneous_trigger_operand: %w", err)
			}
		}
		source, err := r.timerSource(kindName, trigger, scope)
		if err != nil {
			return nil, err
		}
		switch kindName {
		case "after":
			return hsm.After(source.duration), nil
		case "every":
			return hsm.Every(source.duration), nil
		case "at":
			return hsm.At(source.timepoint), nil
		}
	case "exit_point":
		return nil, fmt.Errorf("exit_point trigger should be built by buildTransitionExpanded")
	default:
		return nil, fmt.Errorf("unsupported trigger kind %q", kindName)
	}
	return nil, fmt.Errorf("unsupported trigger kind %q", kindName)
}

func (r *runner) exitPointName(trigger anyMap) (string, error) {
	if err := validateOnlyKeys(trigger, "kind", "exit_point"); err != nil {
		return "", fmt.Errorf("extraneous_trigger_operand: %w", err)
	}
	name, err := requireString(trigger, "exit_point")
	if err != nil {
		return "", fmt.Errorf("missing_trigger_operand: %w", err)
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid_name: exit point %q", name)
	}
	return name, nil
}

func (r *runner) validateExitPointHandler(boundaryPath, name string) error {
	machineName := r.submachineModels[boundaryPath]
	if machineName == "" {
		return fmt.Errorf("invalid_exit_point_usage: source %s is not a submachine", boundaryPath)
	}
	modelIR := r.modelIRs[machineName]
	if r.modelDeclaresExitPoint(modelIR, name, map[string]bool{}) {
		return nil
	}
	return fmt.Errorf("missing_exit_point: %s", name)
}

func (r *runner) resolveExitPoint(boundaryPath, name string) (exitPointDef, error) {
	direct := make([]string, 0)
	nested := make([]string, 0)
	for exitPointPath, def := range r.exitPoints {
		if def.name != name {
			continue
		}
		if exitPointPath != boundaryPath && !strings.HasPrefix(exitPointPath, boundaryPath+"/") {
			continue
		}
		owner := path.Dir(exitPointPath)
		if owner == boundaryPath || path.Dir(owner) == boundaryPath {
			direct = append(direct, exitPointPath)
		} else {
			nested = append(nested, exitPointPath)
		}
	}
	sort.Strings(direct)
	sort.Strings(nested)
	if len(direct) > 0 {
		return r.exitPoints[direct[0]], nil
	}
	if len(nested) > 0 {
		return r.exitPoints[nested[0]], nil
	}
	return exitPointDef{}, fmt.Errorf("missing_exit_point: %s", name)
}

func (r *runner) modelDeclaresExitPoint(modelIR anyMap, name string, seen map[string]bool) bool {
	modelName, _ := modelIR["name"].(string)
	if modelName == "" || seen[modelName] {
		return false
	}
	seen[modelName] = true
	for _, raw := range arrayAny(modelIR["exit_points"]) {
		exitPoint := object(raw)
		if exitName, _ := exitPoint["name"].(string); exitName == name {
			return true
		}
	}
	for _, raw := range arrayAny(modelIR["states"]) {
		stateIR := object(raw)
		if kindName, _ := stateIR["kind"].(string); kindName != "submachine" {
			if r.modelDeclaresExitPointInStates(arrayAny(stateIR["states"]), name, seen) {
				return true
			}
			continue
		}
		machineName, _ := stateIR["machine"].(string)
		if child := r.modelIRs[machineName]; child != nil && r.modelDeclaresExitPoint(child, name, seen) {
			return true
		}
	}
	return false
}

func (r *runner) modelDeclaresExitPointInStates(states []any, name string, seen map[string]bool) bool {
	for _, raw := range states {
		stateIR := object(raw)
		if kindName, _ := stateIR["kind"].(string); kindName == "submachine" {
			machineName, _ := stateIR["machine"].(string)
			if child := r.modelIRs[machineName]; child != nil && r.modelDeclaresExitPoint(child, name, seen) {
				return true
			}
			continue
		}
		if r.modelDeclaresExitPointInStates(arrayAny(stateIR["states"]), name, seen) {
			return true
		}
	}
	return false
}

func isExitPointTrigger(transitionIR anyMap) bool {
	trigger := object(transitionIR["trigger"])
	return trigger != nil && trigger["kind"] == "exit_point"
}

func (r *runner) transitionEventNames(transitionIR anyMap, scope string) ([]string, error) {
	trigger := object(transitionIR["trigger"])
	if trigger == nil {
		if on, ok := transitionIR["on"]; ok {
			trigger = anyMap{"kind": "on", "event": on}
		}
	}
	if trigger == nil {
		return nil, nil
	}
	switch trigger["kind"] {
	case "on":
		events := []any{}
		if rawEvents, hasEvents := trigger["events"]; hasEvents {
			events = arrayAny(rawEvents)
		} else {
			events = []any{trigger["event"]}
		}
		names := make([]string, 0, len(events))
		for _, event := range events {
			name, err := eventNameValue(event)
			if err != nil {
				return nil, err
			}
			names = append(names, name)
		}
		return names, nil
	case "on_call":
		name, _ := trigger["operation"].(string)
		if name == "" {
			return nil, nil
		}
		operationName := r.operationNameInScopePath(scope, name)
		if operationName == "" {
			operationName = path.Join(rootPath(scope), name)
		}
		return []string{operationName}, nil
	case "on_set":
		name, _ := trigger["attribute"].(string)
		if name == "" {
			return nil, nil
		}
		name = r.attrNameInScope(scope, name)
		return []string{name}, nil
	case "when":
		if name, _ := trigger["attribute"].(string); name != "" {
			return []string{r.attrNameInScope(scope, name)}, nil
		}
		if _, ok := trigger["behavior"].(string); ok {
			return r.visibleAttrEventNames(scope), nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func (r *runner) buildWhenTrigger(trigger anyMap, ownerPath, scope string) (hsm.RedefinableElement, []string, func(context.Context, *confInstance, hsm.Event) bool, error) {
	if err := validateOnlyKeys(trigger, "kind", "attribute", "behavior"); err != nil {
		return nil, nil, nil, fmt.Errorf("extraneous_trigger_operand: %w", err)
	}
	if scope == "" {
		scope = "/" + rootName(ownerPath)
	}
	if attr, ok := trigger["attribute"].(string); ok && attr != "" {
		eventName := r.attrNameInScope(scope, attr)
		r.traceSetAttrs[eventName] = true
		return hsm.OnSet(localBuilderName(eventName)), []string{eventName}, nil, nil
	}
	if behavior, ok := trigger["behavior"].(string); ok && behavior != "" {
		eventNames := r.visibleAttrEventNames(scope)
		if len(eventNames) == 0 {
			return nil, nil, nil, fmt.Errorf("when behavior trigger requires at least one model attribute")
		}
		parts := make([]hsm.RedefinableElement, 0, len(eventNames))
		for _, eventName := range eventNames {
			r.traceSetAttrs[eventName] = true
			parts = append(parts, hsm.OnSet(localBuilderName(eventName)))
		}
		return combineRedefinable(parts), eventNames, func(ctx context.Context, sm *confInstance, event hsm.Event) bool {
			ctx = withBehaviorScope(ctx, scope)
			value, err := r.executeBehavior(ctx, sm, event, behavior, "when")
			if err != nil {
				r.recordExpectedError(err)
				panic(err)
			}
			return truthy(value)
		}, nil
	}
	return nil, nil, nil, fmt.Errorf("when trigger requires attribute or behavior")
}

func (r *runner) visibleAttrEventNames(scope string) []string {
	seen := map[string]bool{}
	eventNames := []string{}
	root := rootPath(scope)
	for current := scope; current != "" && current != "." && current != "/"; {
		for _, attr := range r.scopedAttrs[current] {
			eventName := path.Join(root, attr)
			if !seen[eventName] {
				seen[eventName] = true
				eventNames = append(eventNames, eventName)
			}
		}
		next := path.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	sort.Strings(eventNames)
	return eventNames
}

func combineRedefinable(parts []hsm.RedefinableElement) hsm.RedefinableElement {
	return func(model *hsm.Model, stack []hsm.Element) hsm.Element {
		var owner hsm.Element
		for _, part := range parts {
			owner = part(model, stack)
		}
		return owner
	}
}

type timerSource struct {
	duration  func(context.Context, hsm.Instance, hsm.Event) time.Duration
	timepoint func(context.Context, hsm.Instance, hsm.Event) time.Time
}

func (r *runner) timerSource(kindName string, trigger anyMap, scope string) (timerSource, error) {
	if raw, ok := trigger["duration_ms"]; ok {
		duration := durationMillis(raw)
		return timerSource{
			duration: func(_ context.Context, _ hsm.Instance, event hsm.Event) time.Duration {
				r.noteTimerName(event.Name)
				r.noteTimerScheduled(kindName)
				return positiveTimerDuration(duration)
			},
		}, nil
	}
	if raw, ok := trigger["time_ms"]; ok {
		target := durationMillis(raw)
		return timerSource{
			timepoint: func(_ context.Context, _ hsm.Instance, event hsm.Event) time.Time {
				r.noteTimerName(event.Name)
				r.noteTimerScheduled(kindName)
				remaining := target - r.clock.now
				return time.Now().Add(positiveTimerDuration(remaining))
			},
		}, nil
	}
	if attr, ok := trigger["attribute"].(string); ok {
		attrName := r.attrNameInScope(scope, attr)
		r.traceSetAttrs[attrName] = true
		return timerSource{
			duration: func(ctx context.Context, sm hsm.Instance, event hsm.Event) time.Duration {
				return r.timerDurationFromAttribute(ctx, sm, event, attrName, kindName)
			},
			timepoint: func(ctx context.Context, sm hsm.Instance, event hsm.Event) time.Time {
				return time.Now().Add(r.timerDurationFromAttribute(ctx, sm, event, attrName, kindName))
			},
		}, nil
	}
	if behavior, ok := trigger["behavior"].(string); ok {
		traceSchedule := r.behaviorIsSilentTimerSource(behavior)
		return timerSource{
			duration: func(ctx context.Context, sm hsm.Instance, event hsm.Event) time.Duration {
				return r.timerDurationFromBehavior(ctx, sm, event, behavior, scope, kindName, traceSchedule)
			},
			timepoint: func(ctx context.Context, sm hsm.Instance, event hsm.Event) time.Time {
				return time.Now().Add(r.timerDurationFromBehavior(ctx, sm, event, behavior, scope, kindName, traceSchedule))
			},
		}, nil
	}
	return timerSource{}, fmt.Errorf("%s trigger requires timer source", kindName)
}

func (r *runner) timerDurationFromAttribute(ctx context.Context, sm hsm.Instance, event hsm.Event, attrName, kindName string) time.Duration {
	value, _ := hsm.Get(ctx, sm, attrName)
	return r.positiveTimerDurationFromValue(value, event, kindName, true)
}

func (r *runner) timerDurationFromBehavior(ctx context.Context, sm hsm.Instance, event hsm.Event, behavior, scope, kindName string, traceSchedule bool) time.Duration {
	ctx = withBehaviorScope(ctx, scope)
	value, err := r.executeBehavior(ctx, sm.(*confInstance), event, behavior, "timer")
	if err != nil {
		r.recordExpectedError(err)
		panic(err)
	}
	return r.positiveTimerDurationFromValue(value, event, kindName, traceSchedule)
}

func (r *runner) positiveTimerDurationFromValue(value any, event hsm.Event, kindName string, traceSchedule bool) time.Duration {
	duration, err := timerValueDuration(value)
	if err != nil {
		conformanceErr := conformanceError{code: "timer_error", message: err.Error()}
		r.recordExpectedError(conformanceErr)
		panic(conformanceErr)
	}
	r.noteTimerName(event.Name)
	if traceSchedule {
		r.noteTimerScheduled(kindName)
	} else {
		r.noteTimerKind(kindName)
	}
	return positiveTimerDuration(duration)
}

func (r *runner) noteTimerScheduled(kindName string) {
	r.timerMu.Lock()
	defer r.timerMu.Unlock()
	r.pendingTimerScheduled++
	r.noteTimerKindLocked(kindName)
}

func (r *runner) noteTimerKind(kindName string) {
	r.timerMu.Lock()
	defer r.timerMu.Unlock()
	r.noteTimerKindLocked(kindName)
}

func (r *runner) noteTimerName(eventName string) {
	if eventName == "" {
		return
	}
	r.timerMu.Lock()
	defer r.timerMu.Unlock()
	if gid := currentGoroutineID(); gid != 0 {
		r.pendingTimerNamesByG[gid] = append(r.pendingTimerNamesByG[gid], eventName)
	}
}

func (r *runner) nextTimerName() string {
	r.timerMu.Lock()
	defer r.timerMu.Unlock()
	gid := currentGoroutineID()
	if gid == 0 {
		return ""
	}
	names := r.pendingTimerNamesByG[gid]
	if len(names) == 0 {
		return ""
	}
	name := names[0]
	names = names[1:]
	if len(names) == 0 {
		delete(r.pendingTimerNamesByG, gid)
	} else {
		r.pendingTimerNamesByG[gid] = names
	}
	return name
}

func (r *runner) noteTimerKindLocked(kindName string) {
	if !r.usesConfigClock {
		return
	}
	if gid := currentGoroutineID(); gid != 0 {
		r.pendingTimerKindsByG[gid] = append(r.pendingTimerKindsByG[gid], kindName)
	}
	r.pendingTimerKinds = append(r.pendingTimerKinds, kindName)
}

func (r *runner) nextTimerKind() string {
	r.timerMu.Lock()
	defer r.timerMu.Unlock()
	if gid := currentGoroutineID(); gid != 0 {
		kinds := r.pendingTimerKindsByG[gid]
		if len(kinds) > 0 {
			kindName := kinds[0]
			kinds = kinds[1:]
			if len(kinds) == 0 {
				delete(r.pendingTimerKindsByG, gid)
			} else {
				r.pendingTimerKindsByG[gid] = kinds
			}
			r.removeGlobalTimerKindLocked(kindName)
			return kindName
		}
	}
	if len(r.pendingTimerKinds) == 0 {
		return ""
	}
	kindName := r.pendingTimerKinds[0]
	r.pendingTimerKinds = r.pendingTimerKinds[1:]
	r.removeGoroutineTimerKindLocked(kindName)
	return kindName
}

func (r *runner) removeGlobalTimerKindLocked(kindName string) {
	for index, pending := range r.pendingTimerKinds {
		if pending != kindName {
			continue
		}
		r.pendingTimerKinds = append(r.pendingTimerKinds[:index], r.pendingTimerKinds[index+1:]...)
		return
	}
}

func (r *runner) removeGoroutineTimerKindLocked(kindName string) {
	for gid, kinds := range r.pendingTimerKindsByG {
		for index, pending := range kinds {
			if pending != kindName {
				continue
			}
			kinds = append(kinds[:index], kinds[index+1:]...)
			if len(kinds) == 0 {
				delete(r.pendingTimerKindsByG, gid)
			} else {
				r.pendingTimerKindsByG[gid] = kinds
			}
			return
		}
	}
}

// currentGoroutineID is isolated to the conformance runner's fake-clock
// correlation path. The production runtime does not depend on goroutine IDs;
// this lets the runner pair a timer source callback with the immediately
// following clock timer creation when multiple timer sources are active.
func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 || fields[0] != "goroutine" {
		return 0
	}
	id, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func (r *runner) behaviorIsSilentTimerSource(behaviorID string) bool {
	program, ok := r.caseData.Behaviors[behaviorID]
	if !ok {
		return false
	}
	visible := map[string]bool{
		"trace":                    true,
		"set_attr":                 true,
		"set_attr_from_event_data": true,
		"dispatch":                 true,
		"call":                     true,
		"snapshot":                 true,
		"raise":                    true,
		"sleep":                    true,
		"yield":                    true,
	}
	for _, operation := range program {
		name, _ := operation["op"].(string)
		if visible[name] {
			return false
		}
	}
	return true
}

func (r *runner) flushTimerScheduled(count int) {
	r.timerMu.Lock()
	defer r.timerMu.Unlock()
	if count < 0 || count > r.pendingTimerScheduled {
		count = r.pendingTimerScheduled
	}
	for i := 0; i < count; i++ {
		r.trace = append(r.trace, anyMap{"type": "timer_scheduled"})
	}
	r.pendingTimerScheduled -= count
}

func (r *runner) timerFiredGuard(id string, hasGuard bool, scope, canonicalEventName string) func(context.Context, *confInstance, hsm.Event) bool {
	return func(ctx context.Context, sm *confInstance, event hsm.Event) bool {
		r.bindTimerEventName(event.Name, canonicalEventName)
		r.flushTimerScheduled(1)
		if !hasGuard {
			r.trace = append(r.trace, anyMap{"type": "timer_fired"})
			return true
		}
		firedIndex := len(r.trace)
		defer func() {
			if recovered := recover(); recovered != nil {
				r.insertTrace(firedIndex, anyMap{"type": "timer_fired"})
				panic(recovered)
			}
		}()
		result := r.guardCallback(id, scope)(ctx, sm, event)
		if result {
			r.trace = append(r.trace, anyMap{"type": "timer_fired"})
		} else {
			r.insertTrace(firedIndex, anyMap{"type": "timer_fired"})
		}
		return result
	}
}

func (r *runner) bindTimerEventName(runtimeName, canonicalName string) {
	if runtimeName == "" || canonicalName == "" {
		return
	}
	r.timerNameMu.Lock()
	if existing, exists := r.timerNameCache[runtimeName]; !exists {
		r.timerNameCache[runtimeName] = canonicalName
	} else if existing != canonicalName {
		r.timerNameMu.Unlock()
		r.recordError(conformanceError{code: "timer_binding_error", message: "conflicting timer binding for " + runtimeName + ": " + existing + " vs " + canonicalName})
		return
	}
	r.timerNameMu.Unlock()
}

func (r *runner) insertTrace(index int, event anyMap) {
	if index < 0 || index >= len(r.trace) {
		r.trace = append(r.trace, event)
		return
	}
	r.trace = append(r.trace, nil)
	copy(r.trace[index+1:], r.trace[index:])
	r.trace[index] = event
}

func (r *runner) buildInstances(defaultModel *hsm.FinalizedModel) error {
	if len(r.caseData.Instances) == 0 {
		r.instances["default"] = hsm.New(&confInstance{}, defaultModel, hsm.Config{ID: "default", Clock: r.clock.Clock()})
		r.instanceOrder = append(r.instanceOrder, "default")
		return nil
	}
	for _, instanceIR := range r.caseData.Instances {
		id, err := requireString(instanceIR, "id")
		if err != nil {
			return err
		}
		model := defaultModel
		if modelName, ok := instanceIR["model"].(string); ok && modelName != "" {
			childIR := r.modelIRs[modelName]
			if childIR == nil {
				r.invalidInstanceModels[id] = modelName
			} else {
				childModel, err := r.buildModel(childIR)
				if err != nil {
					return err
				}
				model = childModel
			}
		}
		config := hsm.Config{ID: id, Clock: r.clock.Clock()}
		if value, ok := instanceIR["data"]; ok {
			config.Data = normalizeJSONValue(value)
			r.startData[id] = config.Data
		}
		if configIR := object(instanceIR["config"]); configIR != nil {
			if name, ok := configIR["Name"].(string); ok {
				config.Name = name
			} else if name, ok := configIR["name"].(string); ok {
				config.Name = name
			}
			if value, ok := configIR["Data"]; ok {
				config.Data = normalizeJSONValue(value)
				r.startData[id] = config.Data
			} else if value, ok := configIR["data"]; ok {
				config.Data = normalizeJSONValue(value)
				r.startData[id] = config.Data
			}
			if queueName, ok := configIR["queue"].(string); ok && queueName != "" {
				queue, err := r.configQueue(queueName, id)
				if err != nil {
					return err
				}
				config.Queue = queue
				r.instanceQueues[id] = queueName
			}
			if clockName, ok := configIR["clock"].(string); ok && clockName != "" {
				clock, err := r.configClock(clockName)
				if err != nil {
					return err
				}
				config.Clock = clock
			}
			if clockName, ok := configIR["Clock"].(string); ok && clockName != "" {
				clock, err := r.configClock(clockName)
				if err != nil {
					return err
				}
				config.Clock = clock
			}
		}
		r.instances[id] = hsm.New(&confInstance{}, model, config)
		r.instanceOrder = append(r.instanceOrder, id)
	}
	return nil
}

func (r *runner) configQueue(name, instanceID string) (hsm.Queue, error) {
	switch name {
	case "trace_fifo":
		return r.traceQueue(instanceID, false, nil, nil), nil
	case "trace_lifo":
		return r.traceQueue(instanceID, true, nil, nil), nil
	case "len_seven":
		return r.traceQueue(instanceID, false, func(context.Context) (int, error) {
			return 7, nil
		}, nil), nil
	case "push_error":
		return r.pushErrorQueue(instanceID), nil
	case "pop_error_once":
		return r.popErrorOnceQueue(instanceID), nil
	case "len_error_once":
		return r.lenErrorOnceQueue(instanceID), nil
	default:
		return hsm.Queue{}, fmt.Errorf("unsupported_queue: configured queue %q", name)
	}
}

func (r *runner) traceEventName(event hsm.Event) string {
	if strings.HasPrefix(event.Name, "@call:/") {
		return "@call:" + path.Base(event.Name)
	}
	if name, ok := r.canonicalTimerEventName(event.Name); ok {
		return name
	}
	if name := r.timerEventNameForRuntimeEvent(event.Name); name != "" {
		r.bindTimerEventName(event.Name, name)
		return name
	}
	if event.Kind != hsm.TimeEventKind {
		return event.Name
	}
	r.recordError(conformanceError{code: "timer_binding_error", message: "unbound runtime timer event " + event.Name})
	return event.Name
}

func (r *runner) canonicalTimerEventName(eventName string) (string, bool) {
	r.timerNameMu.Lock()
	defer r.timerNameMu.Unlock()
	if name, ok := r.timerNameCache[eventName]; ok {
		return name, true
	}
	return "", false
}

func (r *runner) timerRuntimeNameBound(eventName string) bool {
	r.timerNameMu.Lock()
	defer r.timerNameMu.Unlock()
	_, ok := r.timerNameCache[eventName]
	return ok
}

func (r *runner) bindModelTimerEventNames(model *hsm.FinalizedModel) {
	if model == nil {
		return
	}
	if !r.hasIndexedTimerEvents() {
		return
	}
	type pendingTimerBinding struct {
		transitionName string
		eventName      string
	}
	pendingByOwner := map[string][]pendingTimerBinding{}
	for _, transition := range model.TransitionSnapshots() {
		for _, eventName := range transition.Events {
			canonical := r.timerEventNameForTransition(transition.Name)
			if canonical != "" {
				r.bindTimerEventName(eventName, canonical)
				continue
			}
			canonical = r.timerEventNameForRuntimeEvent(eventName)
			if canonical != "" {
				r.bindTimerEventName(eventName, canonical)
				continue
			}
			if !isRuntimeTimerEventName(eventName) || len(r.timerEventsByOwner[transition.Source]) == 0 {
				continue
			}
			pendingByOwner[transition.Source] = append(pendingByOwner[transition.Source], pendingTimerBinding{
				transitionName: transition.Name,
				eventName:      eventName,
			})
		}
	}
	for owner, bindings := range pendingByOwner {
		sort.SliceStable(bindings, func(i, j int) bool {
			return bindings[i].transitionName < bindings[j].transitionName
		})
		defs := r.timerEventsByOwner[owner]
		for index, binding := range bindings {
			if r.timerRuntimeNameBound(binding.eventName) {
				continue
			}
			if index >= len(defs) {
				r.recordError(conformanceError{code: "timer_binding_error", message: "runtime exposed more timer events than indexed IR timers for " + owner})
				continue
			}
			r.bindTimerEventName(binding.eventName, defs[index].name)
		}
	}
}

func (r *runner) hasIndexedTimerEvents() bool {
	for _, defs := range r.timerEventsByOwner {
		if len(defs) > 0 {
			return true
		}
	}
	return false
}

func (r *runner) timerEventNameForTransition(transitionName string) string {
	if transitionName == "" {
		return ""
	}
	for _, defs := range r.timerEventsByOwner {
		for _, def := range defs {
			if path.Dir(path.Dir(def.name)) == transitionName {
				return def.name
			}
		}
	}
	return ""
}

func (r *runner) timerEventNameForRuntimeEvent(eventName string) string {
	if eventName == "" {
		return ""
	}
	part := path.Base(eventName)
	if part == "duration" || part == "timepoint" {
		if name := r.timerEventNameForTransition(path.Dir(eventName)); name != "" {
			return name
		}
		owner := path.Dir(path.Dir(eventName))
		return r.singleTimerEventNameByPart(owner, part)
	}
	if name := r.timerEventNameForTransition(path.Dir(path.Dir(eventName))); name != "" {
		return name
	}
	owner := path.Dir(path.Dir(path.Dir(eventName)))
	part = path.Base(path.Dir(eventName))
	return r.singleTimerEventNameByPart(owner, part)
}

func (r *runner) singleTimerEventNameByPart(owner, part string) string {
	matches := []string{}
	for _, def := range r.timerEventsByOwner[owner] {
		if timerPartForKind(def.kind) == part {
			matches = append(matches, def.name)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func isRuntimeTimerEventName(eventName string) bool {
	if eventName == "" {
		return false
	}
	base := path.Base(eventName)
	transitionName := ""
	if base == "duration" || base == "timepoint" {
		transitionName = path.Base(path.Dir(eventName))
	} else {
		if _, err := strconv.Atoi(base); err != nil {
			return false
		}
		transitionName = path.Base(path.Dir(path.Dir(eventName)))
	}
	return strings.HasPrefix(transitionName, "transition_")
}

func timerPartForKind(kindName string) string {
	switch kindName {
	case "after", "every":
		return "duration"
	case "at":
		return "timepoint"
	default:
		return ""
	}
}

func (r *runner) timerEventNameForIR(statePath, kindName string, transitionOrdinal int) string {
	for _, def := range r.timerEventsByOwner[statePath] {
		if def.kind == kindName && def.transitionOrdinal == transitionOrdinal {
			return def.name
		}
	}
	return ""
}

func (r *runner) configClock(name string) (hsm.Clock, error) {
	switch name {
	case "trace_no_sleep", "trace_yield_sleep", "trace_nonzero_sleep":
	default:
		return hsm.Clock{}, fmt.Errorf("unsupported clock fixture %q", name)
	}
	r.usesConfigClock = true
	return hsm.Clock{
		NewTimer: func(duration time.Duration) *time.Timer {
			return r.configTimer(name, duration)
		},
	}, nil
}

func (r *runner) configTimer(name string, duration time.Duration) *time.Timer {
	millis := int(math.Round(float64(duration) / float64(time.Millisecond)))
	value := fmt.Sprintf("clock:sleep:%d", millis)
	if name == "trace_nonzero_sleep" && duration > 0 {
		value = "clock:sleep:nonzero"
	}
	r.flushTimerScheduled(1)
	r.trace = append(r.trace, anyMap{"type": "trace", "value": value})
	timerKind := r.nextTimerKind()
	if name == "trace_nonzero_sleep" && duration > 0 && timerKind == "" {
		r.recordError(conformanceError{code: "timer_binding_error", message: "clock timer was created without a matching timer source kind"})
	}
	if name == "trace_yield_sleep" || (name == "trace_nonzero_sleep" && duration > 0 && timerKind != "at") {
		return r.clock.NewTimer(duration)
	}
	if name == "trace_nonzero_sleep" && duration > 0 && timerKind == "at" {
		return r.clock.NewTimer(0)
	}
	return time.NewTimer(0)
}

func (r *runner) traceQueue(instanceID string, lifo bool, lenOverride func(context.Context) (int, error), prePop func(hsm.Event) error) hsm.Queue {
	var mutex sync.Mutex
	events := []hsm.Event{}
	return hsm.Queue{
		Push: func(_ context.Context, event hsm.Event) error {
			mutex.Lock()
			defer mutex.Unlock()
			r.trace = append(r.trace, anyMap{"type": "trace", "value": "queue:push:" + r.traceEventName(event)})
			events = append(events, event)
			return nil
		},
		Pop: func(ctx context.Context) (hsm.Event, bool, error) {
			mutex.Lock()
			if len(events) == 0 {
				mutex.Unlock()
				return hsm.Event{}, false, nil
			}
			var event hsm.Event
			if lifo {
				event = events[len(events)-1]
			} else {
				event = events[0]
			}
			if prePop != nil {
				if err := prePop(event); err != nil {
					mutex.Unlock()
					return hsm.Event{}, false, err
				}
			}
			if lifo {
				events = events[:len(events)-1]
			} else {
				events = events[1:]
			}
			key := queueGateKey{instanceID: instanceID, eventName: event.Name}
			claim := r.queuePopReleases(key)
			if claim.beforeRelease != nil {
				mutex.Unlock()
				select {
				case <-claim.beforeRelease:
				case <-ctx.Done():
					return hsm.Event{}, false, ctx.Err()
				}
				mutex.Lock()
			}
			r.trace = append(r.trace, anyMap{"type": "trace", "value": "queue:pop:" + r.traceEventName(event)})
			queuePopSeen(claim)
			if claim.afterRelease != nil {
				mutex.Unlock()
				select {
				case <-claim.afterRelease:
				case <-ctx.Done():
					return hsm.Event{}, false, ctx.Err()
				}
				mutex.Lock()
			}
			mutex.Unlock()
			return event, true, nil
		},
		Len: func(ctx context.Context) (int, error) {
			if lenOverride != nil {
				return lenOverride(ctx)
			}
			mutex.Lock()
			defer mutex.Unlock()
			return len(events), nil
		},
	}
}

func (r *runner) pushErrorQueue(instanceID string) hsm.Queue {
	queue := r.traceQueue(instanceID, false, nil, nil)
	queue.Push = func(_ context.Context, event hsm.Event) error {
		err := conformanceError{code: "runtime_error", message: "queue push error"}
		r.trace = append(r.trace, anyMap{"type": "trace", "value": "queue:push-error:" + event.Name})
		r.recordExpectedError(err)
		return err
	}
	return queue
}

func (r *runner) popErrorOnceQueue(instanceID string) hsm.Queue {
	failed := false
	return r.traceQueue(instanceID, false, nil, func(hsm.Event) error {
		if failed {
			return nil
		}
		failed = true
		r.trace = append(r.trace, anyMap{"type": "trace", "value": "queue:pop-error"})
		return fmt.Errorf("queue pop error")
	})
}

func (r *runner) lenErrorOnceQueue(instanceID string) hsm.Queue {
	lenFailed := false
	return r.traceQueue(instanceID, false, func(context.Context) (int, error) {
		if !lenFailed {
			lenFailed = true
			r.trace = append(r.trace, anyMap{"type": "trace", "value": "queue:len-error"})
			return 0, fmt.Errorf("queue len error")
		}
		return 0, nil
	}, nil)
}

func (r *runner) buildGroups() error {
	if err := r.requireUniqueGroupIDs(); err != nil {
		return err
	}
	for _, groupIR := range r.caseData.Groups {
		id, err := requireString(groupIR, "id")
		if err != nil {
			return err
		}
		membersValue, ok := groupIR["members"].([]any)
		if !ok {
			return fmt.Errorf("group.members must be an array")
		}
		values := []any{id}
		memberIDs := []string{}
		seenMembers := map[string]bool{}
		for _, memberAny := range membersValue {
			memberID, err := memberIDValue(memberAny)
			if err != nil {
				return err
			}
			if seenMembers[memberID] {
				return fmt.Errorf("duplicate_group_member: %q", memberID)
			}
			member := r.instances[memberID]
			if member == nil {
				return fmt.Errorf("unknown group member %q", memberID)
			}
			seenMembers[memberID] = true
			memberIDs = append(memberIDs, memberID)
			values = append(values, member)
		}
		if len(membersValue) < 2 {
			return fmt.Errorf("invalid_group_cardinality: group must contain at least two members")
		}
		r.groups[id] = hsm.MakeGroup(values...)
		r.groupMembers[id] = memberIDs
	}
	return nil
}

func (r *runner) requireUniqueGroupIDs() error {
	groupIDs := map[string]bool{}
	for _, groupIR := range r.caseData.Groups {
		id, err := requireString(groupIR, "id")
		if err != nil {
			return err
		}
		if groupIDs[id] {
			return fmt.Errorf("duplicate_group: %q", id)
		}
		groupIDs[id] = true
	}
	return nil
}

func (r *runner) dispatchAll(ctx context.Context, event hsm.Event, sequential bool, current *confInstance) hsm.Completion {
	return r.dispatchInstances(ctx, event, r.instanceOrder, sequential, current)
}

func (r *runner) dispatchGroup(ctx context.Context, event hsm.Event, groupID string, sequential bool, current *confInstance) (hsm.Completion, *conformanceError) {
	memberIDs := r.groupMembers[groupID]
	if !sequential {
		done := make(chan error)
		go func() {
			defer close(done)
			runtimeYield()
			<-r.dispatchInstances(ctx, event, memberIDs, true, current)
		}()
		return hsm.Completion(done), nil
	}
	return r.dispatchInstances(ctx, event, memberIDs, sequential, current), nil
}

func (r *runner) dispatchTo(ctx context.Context, event hsm.Event, ids []string, sequential bool, current *confInstance) hsm.Completion {
	return r.dispatchInstances(ctx, event, ids, sequential, current)
}

func (r *runner) dispatchInstances(ctx context.Context, event hsm.Event, ids []string, wait bool, current *confInstance) hsm.Completion {
	if ctx == nil {
		ctx = r.ctx
	}
	if ctx == nil {
		done := make(chan error)
		r.recordError(conformanceError{code: "runtime_wait_without_context", message: "dispatch " + event.Name})
		close(done)
		return hsm.Completion(done)
	}
	done := make(chan error)
	targets := r.activeDispatchTargets(ids)
	orderedTargets := make([]string, 0, len(targets))
	for _, id := range targets {
		if current != nil && r.instances[id] == current {
			continue
		}
		orderedTargets = append(orderedTargets, id)
	}
	for _, id := range targets {
		if current != nil && r.instances[id] == current {
			orderedTargets = append(orderedTargets, id)
		}
	}
	if wait && r.allTargetsUseConfiguredQueue(targets) {
		gate := r.beginQueueFanoutGate(targets, event.Name)
		for i, id := range targets {
			instance := r.instances[id]
			targetedEvent := r.eventForDispatchTarget(event, id, current)
			r.clearEventMemory(instance, event.Name)
			signal := hsm.Dispatch(ctx, instance, targetedEvent)
			close(gate.entries[i].beforeRelease)
			if err := r.waitQueuePop(gate, queueGateKey{instanceID: id, eventName: event.Name}, ctx); err != nil {
				r.recordError(err)
				r.endQueueFanoutGate(gate)
				close(done)
				return hsm.Completion(done)
			}
			close(gate.entries[i].afterRelease)
			select {
			case err := <-signal:
				if err != nil {
					r.recordError(err)
				}
			case <-ctx.Done():
				r.recordError(conformanceError{code: "runtime_wait_cancelled", message: "queue dispatch " + id + " " + event.Name})
				r.endQueueFanoutGate(gate)
				close(done)
				return hsm.Completion(done)
			}
		}
		r.endQueueFanoutGate(gate)
		close(done)
		return hsm.Completion(done)
	}
	if !wait {
		defer close(done)
		for _, id := range orderedTargets {
			instance := r.instances[id]
			targetedEvent := r.eventForDispatchTarget(event, id, current)
			r.clearEventMemory(instance, event.Name)
			if current != nil && instance == current {
				_ = hsm.Dispatch(ctx, instance, targetedEvent)
				continue
			}
			if r.instanceQueues[id] != "" {
				gate := r.beginQueueFanoutGate([]string{id}, event.Name)
				signal := hsm.Dispatch(ctx, instance, targetedEvent)
				close(gate.entries[0].beforeRelease)
				if err := r.waitQueuePop(gate, queueGateKey{instanceID: id, eventName: event.Name}, ctx); err != nil {
					r.recordError(err)
					r.endQueueFanoutGate(gate)
					return hsm.Completion(done)
				}
				close(gate.entries[0].afterRelease)
				select {
				case err := <-signal:
					if err != nil {
						r.recordError(err)
					}
				case <-ctx.Done():
					r.recordError(conformanceError{code: "runtime_wait_cancelled", message: "queue dispatch " + id + " " + event.Name})
				}
				r.endQueueFanoutGate(gate)
				continue
			}
			signal := hsm.Dispatch(ctx, instance, targetedEvent)
			select {
			case err := <-signal:
				if err != nil {
					r.recordError(err)
				}
			case <-ctx.Done():
				r.recordError(conformanceError{code: "runtime_wait_cancelled", message: "dispatch " + event.Name})
				return hsm.Completion(done)
			}
		}
		return hsm.Completion(done)
	}
	go func() {
		defer close(done)
		for _, id := range targets {
			instance := r.instances[id]
			targetedEvent := r.eventForDispatchTarget(event, id, current)
			r.clearEventMemory(instance, event.Name)
			select {
			case err := <-hsm.Dispatch(ctx, instance, targetedEvent):
				if err != nil {
					r.recordError(err)
				}
			case <-ctx.Done():
				r.recordError(conformanceError{code: "runtime_wait_cancelled", message: "dispatch " + event.Name})
				return
			}
		}
	}()
	return hsm.Completion(done)
}

func (r *runner) eventForDispatchTarget(event hsm.Event, targetID string, current *confInstance) hsm.Event {
	targeted := event
	if targeted.Source == "" && current != nil {
		targeted.Source = r.instanceIDFor(current)
	}
	if targeted.Target == "" {
		targeted.Target = targetID
	}
	return targeted
}

func (r *runner) instanceIDFor(instance *confInstance) string {
	for id, candidate := range r.instances {
		if candidate == instance {
			return id
		}
	}
	return ""
}

func (r *runner) addPendingWork(done hsm.Completion) {
	if done == nil {
		return
	}
	r.pendingWorkMu.Lock()
	r.pendingWork = append(r.pendingWork, done)
	r.pendingWorkMu.Unlock()
}

func (r *runner) activeDispatchTargets(ids []string) []string {
	targets := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !r.started[id] || r.instances[id] == nil {
			continue
		}
		targets = append(targets, id)
	}
	return targets
}

func (r *runner) targetsUseConfiguredQueue(ids []string) bool {
	for _, id := range ids {
		if r.instanceQueues[id] != "" {
			return true
		}
	}
	return false
}

func (r *runner) allTargetsUseConfiguredQueue(ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if r.instanceQueues[id] == "" {
			return false
		}
	}
	return true
}

func (r *runner) configuredQueueTargets(ids []string) []string {
	targets := make([]string, 0, len(ids))
	for _, id := range ids {
		if r.instanceQueues[id] != "" {
			targets = append(targets, id)
		}
	}
	return targets
}

func (r *runner) beginQueueFanoutGate(targets []string, eventName string) *queueFanoutGate {
	if len(targets) == 0 {
		return nil
	}
	gate := &queueFanoutGate{
		entries: make([]queueGateEntry, len(targets)),
	}
	for i, id := range targets {
		gate.entries[i] = queueGateEntry{
			key:           queueGateKey{instanceID: id, eventName: eventName},
			beforeRelease: make(chan struct{}),
			afterRelease:  make(chan struct{}),
			popSeen:       make(chan struct{}, 1),
		}
	}
	r.queueGateMu.Lock()
	r.queueGates = append(r.queueGates, gate)
	r.queueGateMu.Unlock()
	return gate
}

func (r *runner) endQueueFanoutGate(gate *queueFanoutGate) {
	if gate == nil {
		return
	}
	r.queueGateMu.Lock()
	for i, active := range r.queueGates {
		if active != gate {
			continue
		}
		r.queueGates = append(r.queueGates[:i], r.queueGates[i+1:]...)
		break
	}
	r.queueGateMu.Unlock()
}

func (r *runner) queuePopReleases(key queueGateKey) queueGateClaim {
	r.queueGateMu.Lock()
	defer r.queueGateMu.Unlock()
	for _, gate := range r.queueGates {
		for i := range gate.entries {
			entry := &gate.entries[i]
			if entry.claimed || entry.key != key {
				continue
			}
			entry.claimed = true
			return queueGateClaim{
				beforeRelease: entry.beforeRelease,
				afterRelease:  entry.afterRelease,
				popSeen:       entry.popSeen,
			}
		}
	}
	return queueGateClaim{}
}

func queuePopSeen(claim queueGateClaim) {
	if claim.popSeen == nil {
		return
	}
	select {
	case claim.popSeen <- struct{}{}:
	default:
	}
}

func (r *runner) waitQueuePop(gate *queueFanoutGate, key queueGateKey, ctx context.Context) error {
	if gate == nil {
		return nil
	}
	entry := gate.entry(key)
	if entry == nil {
		return nil
	}
	return waitGatePopSignal(entry.popSeen, ctx, "queue pop "+key.instanceID+" "+key.eventName)
}

func (r *runner) releaseQueueGateSequential(gate *queueFanoutGate, signals []<-chan struct{}, ctx context.Context) error {
	if gate == nil {
		return nil
	}
	for i := range gate.entries {
		close(gate.entries[i].beforeRelease)
		if err := waitGatePopSignal(gate.entries[i].popSeen, ctx, "queue pop "+gate.entries[i].key.instanceID+" "+gate.entries[i].key.eventName); err != nil {
			return err
		}
		close(gate.entries[i].afterRelease)
		if err := waitQueueDispatchSignal(signals, i, gate.entries[i].key, ctx); err != nil {
			return err
		}
	}
	return nil
}

func (gate *queueFanoutGate) entry(key queueGateKey) *queueGateEntry {
	if gate == nil {
		return nil
	}
	for i := range gate.entries {
		if gate.entries[i].key == key {
			return &gate.entries[i]
		}
	}
	return nil
}

func waitGatePopSignal(popSeen <-chan struct{}, ctx context.Context, label string) error {
	if ctx == nil {
		return conformanceError{code: "runtime_wait_without_context", message: label}
	}
	select {
	case <-popSeen:
		return nil
	case <-ctx.Done():
		return conformanceError{code: "runtime_wait_cancelled", message: label}
	}
}

func (r *runner) releaseQueueGateAfterPop(gate *queueFanoutGate, signals []<-chan struct{}, ctx context.Context) error {
	if gate == nil {
		return nil
	}
	for i := range gate.entries {
		close(gate.entries[i].afterRelease)
		if err := waitQueueDispatchSignal(signals, i, gate.entries[i].key, ctx); err != nil {
			return err
		}
	}
	return nil
}

func waitQueueDispatchSignal(signals []<-chan struct{}, index int, key queueGateKey, ctx context.Context) error {
	if index >= len(signals) || signals[index] == nil {
		return nil
	}
	select {
	case <-signals[index]:
		return nil
	case <-ctx.Done():
		return conformanceError{code: "runtime_wait_cancelled", message: "queue dispatch " + key.instanceID + " " + key.eventName}
	}
}

func (r *runner) traceDeferredDispatch(instance *confInstance, eventName string) bool {
	if instance == nil || eventName == "" || !r.traceContractIncludes("defer") {
		return false
	}
	return r.traceDeferredDispatchAtState(instance, eventName, instance.State())
}

func (r *runner) traceDeferredDispatchAtState(instance *confInstance, eventName, statePath string) bool {
	if instance == nil || eventName == "" || statePath == "" || !r.traceContractIncludes("defer") {
		return false
	}
	deferred := r.eventWouldDeferAtState(statePath, eventName)
	if !deferred {
		return deferred
	}
	if r.hasDeferredEvent(instance, eventName) {
		return true
	}
	r.trace = append(r.trace, anyMap{"type": "defer", "event": eventName})
	r.pendingDeferred = append(r.pendingDeferred, deferredEvent{
		instanceID: r.instanceIDForInstance(instance),
		eventName:  eventName,
		owner:      r.deferOwnerAtState(statePath, eventName),
	})
	return true
}

func (r *runner) deferOwnerAtState(state, eventName string) string {
	if !r.eventWouldDeferAtState(state, eventName) {
		return ""
	}
	return walkStateAncestors(state, func(current string) bool {
		return r.deferEvents[current][eventName]
	})
}

func (r *runner) eventIsDeferred(instance *confInstance, eventName string) bool {
	return instance != nil && r.eventIsDeferredAtState(instance.State(), eventName)
}

func (r *runner) eventIsDeferredAtState(state, eventName string) bool {
	return walkStateAncestors(state, func(current string) bool {
		return r.deferEvents[current][eventName]
	}) != ""
}

func (r *runner) eventWouldDeferAtState(state, eventName string) bool {
	return r.eventIsDeferredAtState(state, eventName) && !r.statePathHasUnguardedEventTransition(state, eventName)
}

func (r *runner) traceDeferredReplay(instance *confInstance, eventName string) {
	if !r.traceContractIncludes("undefer") {
		return
	}
	deferredEventName, ok := r.popDeferredEventForSource(instance, r.eventTransitionSourceAtState(instance.State(), eventName))
	if ok {
		r.trace = append(r.trace, anyMap{"type": "undefer", "event": deferredEventName})
		r.deferReplayBarrier = true
	}
}

func (r *runner) traceDeferredReplayFromBehavior(instance *confInstance) {
	if !r.traceContractIncludes("undefer") {
		return
	}
	eventName, ok := r.popDeferredEvent(instance)
	if ok {
		r.trace = append(r.trace, anyMap{"type": "undefer", "event": eventName})
	}
}

func (r *runner) hasDeferredEvent(instance *confInstance, eventName string) bool {
	instanceID := r.instanceIDForInstance(instance)
	for _, deferred := range r.pendingDeferred {
		if deferred.instanceID == instanceID && deferred.eventName == eventName {
			return true
		}
	}
	return false
}

func (r *runner) popDeferredEvent(instance *confInstance) (string, bool) {
	instanceID := r.instanceIDForInstance(instance)
	for index, deferred := range r.pendingDeferred {
		if deferred.instanceID == instanceID {
			r.pendingDeferred = append(r.pendingDeferred[:index], r.pendingDeferred[index+1:]...)
			return deferred.eventName, true
		}
	}
	return "", false
}

func (r *runner) popDeferredEventForSource(instance *confInstance, source string) (string, bool) {
	instanceID := r.instanceIDForInstance(instance)
	for index := 0; index < len(r.pendingDeferred); index++ {
		deferred := r.pendingDeferred[index]
		if deferred.instanceID != instanceID {
			continue
		}
		if source != "" && !r.deferredEventReplaysFromSource(deferred, source) {
			r.pendingDeferred = append(r.pendingDeferred[:index], r.pendingDeferred[index+1:]...)
			index--
			continue
		}
		r.pendingDeferred = append(r.pendingDeferred[:index], r.pendingDeferred[index+1:]...)
		return deferred.eventName, true
	}
	return "", false
}

func (r *runner) deferredEventReplaysFromSource(deferred deferredEvent, source string) bool {
	if deferred.owner == "" || source == "" {
		return true
	}
	boundary := path.Dir(deferred.owner)
	if boundary == "" || boundary == "." || boundary == "/" || boundary == rootPath(deferred.owner) {
		return true
	}
	return source != boundary && hsm.IsAncestor(boundary, source)
}

func (r *runner) instanceIDForInstance(target *confInstance) string {
	for id, instance := range r.instances {
		if instance == target {
			return id
		}
	}
	return "default"
}

func (r *runner) currentStateHasEventTransition(instance *confInstance, eventName string) bool {
	if instance == nil || eventName == "" {
		return false
	}
	return r.statePathHasEventTransition(instance.State(), eventName)
}

func (r *runner) eventTransitionSourceAtState(statePath, eventName string) string {
	return walkStateAncestors(statePath, func(current string) bool {
		return r.statePathHasEventTransition(current, eventName)
	})
}

func (r *runner) statePathHasEventTransition(statePath, eventName string) bool {
	return r.statePathHasEventTransitionWhere(statePath, eventName, nil)
}

func (r *runner) statePathHasUnguardedEventTransition(statePath, eventName string) bool {
	return r.statePathHasEventTransitionWhere(statePath, eventName, func(transition anyMap) bool {
		return transition["guard"] == nil
	})
}

func (r *runner) statePathHasGuardedEventTransition(statePath, eventName string) bool {
	return r.statePathHasEventTransitionWhere(statePath, eventName, func(transition anyMap) bool {
		return transition["guard"] != nil
	})
}

func (r *runner) statePathHasEventTransitionWhere(statePath, eventName string, predicate func(anyMap) bool) bool {
	stateIR := r.stateIRForPath(statePath)
	if stateIR == nil {
		return false
	}
	for _, transitionAny := range arrayAny(stateIR["transitions"]) {
		transition := object(transitionAny)
		if transition == nil || (predicate != nil && !predicate(transition)) {
			continue
		}
		_, targetRoot := r.snapshotRoots(statePath)
		if r.transitionHandlesEvent(transition, targetRoot, eventName) {
			return true
		}
	}
	return false
}

func (r *runner) transitionHandlesEvent(transition anyMap, targetRoot, eventName string) bool {
	names, err := r.transitionEventNames(transition, targetRoot)
	if err != nil {
		return false
	}
	return containsString(names, eventName)
}

func (r *runner) exitingTimerState(instance *confInstance, eventName string) bool {
	if instance == nil || eventName == "" || !r.traceContractIncludes("timer_cancelled") {
		return false
	}
	active := r.activeStateIRs(instance.State())
	hasTimer := false
	hasEventTransition := false
	for _, stateIR := range active {
		if stateHasTimerTransition(stateIR) {
			hasTimer = true
		}
		if stateHasTargetEventTransition(stateIR, eventName) {
			hasEventTransition = true
		}
	}
	if !hasEventTransition && r.modelHasTargetEventTransitionFromState(instance.State(), eventName) {
		hasEventTransition = true
	}
	return hasTimer && hasEventTransition
}

func (r *runner) activeStateIRs(statePath string) []anyMap {
	parts := strings.Split(strings.Trim(statePath, "/"), "/")
	if len(parts) < 2 {
		return nil
	}
	modelIR := r.modelIRs[parts[0]]
	if modelIR == nil {
		return nil
	}
	states := arrayAny(modelIR["states"])
	active := make([]anyMap, 0, len(parts)-1)
	for _, part := range parts[1:] {
		var found anyMap
		for _, stateAny := range states {
			stateIR := object(stateAny)
			if name, _ := stateIR["name"].(string); name == part {
				found = stateIR
				break
			}
		}
		if found == nil {
			break
		}
		active = append(active, found)
		if kind, _ := found["kind"].(string); kind == "submachine" {
			machineName, _ := found["machine"].(string)
			childIR := r.modelIRs[machineName]
			if childIR == nil {
				break
			}
			states = arrayAny(childIR["states"])
		} else {
			states = arrayAny(found["states"])
		}
	}
	return active
}

func stateHasTimerTransition(stateIR anyMap) bool {
	for _, transitionAny := range arrayAny(stateIR["transitions"]) {
		transition := object(transitionAny)
		if transition == nil {
			continue
		}
		trigger := object(transition["trigger"])
		if trigger == nil {
			continue
		}
		switch trigger["kind"] {
		case "after", "every", "at":
			return true
		}
	}
	return false
}

func stateHasTargetEventTransition(stateIR anyMap, eventName string) bool {
	for _, transitionAny := range arrayAny(stateIR["transitions"]) {
		transition := object(transitionAny)
		if transition == nil || transition["target"] == nil {
			continue
		}
		if on, ok := transition["on"]; ok {
			name, err := eventNameValue(on)
			if err == nil && name == eventName {
				return true
			}
		}
	}
	return false
}

func (r *runner) modelHasTargetEventTransitionFromState(statePath, eventName string) bool {
	modelIR := r.modelIRForPath(statePath)
	if modelIR == nil {
		return false
	}
	sourceRoot, targetRoot := r.snapshotRoots(statePath)
	for _, transitionAny := range arrayAny(modelIR["transitions"]) {
		transition := object(transitionAny)
		if transition == nil || transition["target"] == nil {
			continue
		}
		source, err := requireStringValue(transition["source"])
		if err != nil {
			continue
		}
		sourcePath := resolvePathInScope(source, targetRoot, false, sourceRoot, targetRoot)
		if sourcePath != statePath && !hsm.IsAncestor(sourcePath, statePath) {
			continue
		}
		if r.transitionHandlesEvent(transition, targetRoot, eventName) {
			return true
		}
	}
	return false
}

func (r *runner) stateIRForPath(statePath string) anyMap {
	parts := strings.Split(strings.Trim(statePath, "/"), "/")
	if len(parts) < 2 {
		return nil
	}
	modelIR := r.modelIRs[parts[0]]
	if modelIR == nil {
		return nil
	}
	return r.stateIRForParts(modelIR, parts[1:])
}

func (r *runner) modelIRForPath(statePath string) anyMap {
	parts := strings.Split(strings.Trim(statePath, "/"), "/")
	if len(parts) == 0 {
		return nil
	}
	modelIR := r.modelIRs[parts[0]]
	if modelIR == nil {
		return nil
	}
	for _, part := range parts[1:] {
		var found anyMap
		for _, stateAny := range arrayAny(modelIR["states"]) {
			stateIR := object(stateAny)
			if name, _ := stateIR["name"].(string); name == part {
				found = stateIR
				break
			}
		}
		if found == nil {
			return nil
		}
		if kindName, _ := found["kind"].(string); kindName == "submachine" {
			machineName, _ := found["machine"].(string)
			childIR := r.modelIRs[machineName]
			if childIR == nil {
				return nil
			}
			modelIR = childIR
		}
	}
	return modelIR
}

func (r *runner) stateIRForParts(modelIR anyMap, parts []string) anyMap {
	states := arrayAny(modelIR["states"])
	for index, part := range parts {
		var found anyMap
		for _, stateAny := range states {
			stateIR := object(stateAny)
			if name, _ := stateIR["name"].(string); name == part {
				found = stateIR
				break
			}
		}
		if found == nil {
			return nil
		}
		if index == len(parts)-1 {
			return found
		}
		if kind, _ := found["kind"].(string); kind == "submachine" {
			machineName, _ := found["machine"].(string)
			childIR := r.modelIRs[machineName]
			if childIR == nil {
				return nil
			}
			return r.stateIRForParts(childIR, parts[index+1:])
		}
		states = arrayAny(found["states"])
	}
	return nil
}

func (r *runner) shouldTraceScriptSet(name string) bool {
	if r.traceContractIncludes("set") ||
		containsString(r.caseData.Features, "on_set") ||
		containsString(r.caseData.Features, "when") ||
		(containsString(r.caseData.Features, "timer") && containsString(r.caseData.Features, "attribute")) {
		return true
	}
	if r.traceSetAttrs[name] {
		return true
	}
	for attrName := range r.traceSetAttrs {
		if path.Base(attrName) == name {
			return true
		}
	}
	return false
}

func (r *runner) validateScriptSet(name string, value any) *conformanceError {
	spec, ok := r.attrSpecForName(name)
	if !ok {
		return &conformanceError{code: "attribute_error", message: name}
	}
	if spec.typ != "" && spec.typ != "any" && !valueMatchesAttrType(value, spec.typ) {
		return &conformanceError{code: "attribute_error", message: name}
	}
	return nil
}

func (r *runner) attrSpecForName(name string) (attrSpec, bool) {
	spec, ok := r.attrSpecs[name]
	if ok {
		return spec, true
	}
	for attrName, attrSpec := range r.attrSpecs {
		if path.Base(attrName) == name {
			return attrSpec, true
		}
	}
	return attrSpec{}, false
}

func (r *runner) attributeBuilder(name string, spec anyMap) hsm.RedefinableElement {
	typ := hsmAttributeType(spec["type"])
	if value, ok := spec["default"]; ok {
		defaultValue := r.runtimeAttrValue(name, normalizeJSONValue(value))
		if typ != nil {
			return hsm.Attribute(name, typ, defaultValue)
		}
		return hsm.Attribute(name, defaultValue)
	}
	if typ != nil {
		return hsm.Attribute(name, typ)
	}
	return hsm.Attribute(name)
}

func (r *runner) runtimeAttrValue(name string, value any) any {
	spec, ok := r.attrSpecForName(name)
	if !ok || spec.typ != "any" {
		return value
	}
	if _, ok := value.(dynamicAnyValue); ok {
		return value
	}
	return dynamicAnyValue{Value: normalizeJSONValue(value)}
}

func hsmAttributeType(value any) reflect.Type {
	typ, _ := value.(string)
	switch typ {
	case "string":
		return hsm.AttributeType[string]()
	case "boolean", "bool":
		return hsm.AttributeType[bool]()
	case "number":
		return hsm.AttributeType[int]()
	case "array":
		return hsm.AttributeType[[]any]()
	case "object":
		return hsm.AttributeType[anyMap]()
	case "any", "duration_ms", "time_ms":
		return hsm.AttributeType[any]()
	default:
		return nil
	}
}

func (r *runner) executeStep(step op) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			var conformanceErr conformanceError
			switch value := recovered.(type) {
			case *conformanceError:
				if value != nil {
					r.recordError(value)
					err = nil
					return
				}
			case conformanceError:
				r.recordError(value)
				err = nil
				return
			case error:
				if errors.As(value, &conformanceErr) {
					r.recordError(conformanceErr)
					err = nil
					return
				}
			}
			panic(recovered)
		}
	}()
	switch step["op"] {
	case "start":
		instance, err := r.instanceForStep(step)
		if err != nil {
			return err
		}
		id, err := instanceID(step)
		if err != nil {
			return err
		}
		if modelName := r.invalidInstanceModels[id]; modelName != "" {
			err := &conformanceError{code: "model_error", message: modelName}
			r.trace = append(r.trace, anyMap{"type": "error", "code": err.code})
			r.recordError(err)
			break
		}
		if r.started[id] {
			r.recordLifecycleError("already started")
			break
		}
		r.traceLifecycle(step, "start")
		data := r.startData[id]
		if value, ok := step["data"]; ok {
			data = normalizeJSONValue(value)
		}
		r.started[id] = true
		r.ever[id] = true
		hsm.Start(r.ctx, instance, data)
		if err := r.waitFor(r.ctx, instance.Wait(), "start"); err != nil {
			return err
		}
		if containsString(r.caseData.Features, "activity") {
			r.settle()
		}
	case "dispatch":
		instance, err := r.instanceForStep(step)
		if err != nil {
			return err
		}
		event, err := eventFromValue(step["event"])
		if err != nil {
			return err
		}
		r.flushTimerScheduled(-1)
		r.trace = append(r.trace, anyMap{"type": "dispatch", "event": event.Name})
		if r.exitingTimerState(instance, event.Name) {
			r.traceTimerCancelled()
		}
		id, err := instanceID(step)
		if err != nil {
			return err
		}
		if !r.started[id] {
			r.recordLifecycleError("operation requires a started HSM")
			break
		}
		r.lastDispatchQueued = true
		statePath := instance.State()
		eventWillDefer := r.eventWouldDeferAtState(statePath, event.Name)
		traceDeferAfterDispatch := eventWillDefer && r.statePathHasGuardedEventTransition(statePath, event.Name)
		if eventWillDefer && !traceDeferAfterDispatch {
			r.traceDeferredDispatchAtState(instance, event.Name, statePath)
		} else if !eventWillDefer {
			r.traceDeferredReplay(instance, event.Name)
		}
		r.clearEventMemory(instance, event.Name)
		if err := r.waitFor(r.ctx, hsm.Dispatch(r.ctx, instance, event), "dispatch"); err != nil {
			return err
		}
		if traceDeferAfterDispatch {
			r.traceDeferredDispatchAtState(instance, event.Name, statePath)
		}
	case "dispatch_all":
		event, err := eventFromValue(step["event"])
		if err != nil {
			return err
		}
		r.flushTimerScheduled(-1)
		r.stableLabel = "all"
		r.trace = append(r.trace, anyMap{"type": "dispatch", "event": event.Name, "target": "all"})
		deferredTargets := map[string]string{}
		activeTargets := 0
		for _, id := range r.instanceOrder {
			if instance := r.instances[id]; instance != nil && r.started[id] {
				activeTargets++
			}
		}
		for _, id := range r.instanceOrder {
			if instance := r.instances[id]; instance != nil && r.started[id] {
				statePath := instance.State()
				if r.eventWouldDeferAtState(statePath, event.Name) {
					if r.statePathHasGuardedEventTransition(statePath, event.Name) {
						deferredTargets[id] = statePath
					} else {
						r.traceDeferredDispatchAtState(instance, event.Name, statePath)
					}
				} else if activeTargets == 1 {
					r.traceDeferredReplay(instance, event.Name)
				}
			}
		}
		if err := r.waitFor(r.ctx, r.dispatchAll(r.ctx, event, true, nil), "dispatch_all"); err != nil {
			return err
		}
		r.lastDispatchQueued = activeTargets > 0
		for _, id := range r.instanceOrder {
			if statePath, ok := deferredTargets[id]; ok {
				r.traceDeferredDispatchAtState(r.instances[id], event.Name, statePath)
			}
		}
	case "dispatch_to":
		event, err := eventFromValue(step["event"])
		if err != nil {
			return err
		}
		targets, err := r.stepTargets(step)
		if err != nil {
			return err
		}
		traceTarget := any(targets[0])
		if len(targets) > 1 {
			traceTarget = targets
			r.stableLabel = "targets:" + strings.Join(targets, ",")
		} else {
			r.stableLabel = targets[0]
		}
		r.flushTimerScheduled(-1)
		r.trace = append(r.trace, anyMap{"type": "dispatch", "event": event.Name, "target": traceTarget})
		deferredTargets := map[string]string{}
		activeTargets := 0
		for _, target := range targets {
			if instance := r.instances[target]; instance != nil && r.started[target] {
				activeTargets++
			}
		}
		for _, target := range targets {
			if instance := r.instances[target]; instance != nil && r.started[target] {
				statePath := instance.State()
				if r.eventWouldDeferAtState(statePath, event.Name) {
					if r.statePathHasGuardedEventTransition(statePath, event.Name) {
						deferredTargets[target] = statePath
					} else {
						r.traceDeferredDispatchAtState(instance, event.Name, statePath)
					}
				} else if activeTargets == 1 {
					r.traceDeferredReplay(instance, event.Name)
				}
			}
		}
		if err := r.waitFor(r.ctx, r.dispatchTo(r.ctx, event, targets, true, nil), "dispatch_to"); err != nil {
			return err
		}
		r.lastDispatchQueued = activeTargets > 0
		if len(targets) == 1 && containsString(r.caseData.Features, "redefine") {
			r.stableLabel = r.stateFor(targets[0])
		}
		for _, target := range targets {
			if statePath, ok := deferredTargets[target]; ok {
				r.traceDeferredDispatchAtState(r.instances[target], event.Name, statePath)
			}
		}
	case "group_dispatch":
		event, err := eventFromValue(step["event"])
		if err != nil {
			return err
		}
		groupID, err := requireString(stepMap(step), "group")
		if err != nil {
			return err
		}
		r.flushTimerScheduled(-1)
		r.stableLabel = "group:" + groupID
		r.trace = append(r.trace, anyMap{"type": "dispatch", "event": event.Name, "target": groupID})
		group := r.groups[groupID]
		if group == nil {
			conformanceErr := &conformanceError{code: "runtime_error", message: "group " + groupID}
			r.trace = append(r.trace, anyMap{"type": "error", "code": conformanceErr.code})
			r.stableLabel = ""
			r.recordError(conformanceErr)
			break
		}
		deferredTargets := map[string]string{}
		activeTargets := 0
		for _, memberID := range r.groupMembers[groupID] {
			if instance := r.instances[memberID]; instance != nil && r.started[memberID] {
				activeTargets++
			}
		}
		for _, memberID := range r.groupMembers[groupID] {
			if instance := r.instances[memberID]; instance != nil && r.started[memberID] {
				statePath := instance.State()
				if r.eventWouldDeferAtState(statePath, event.Name) {
					if r.statePathHasGuardedEventTransition(statePath, event.Name) {
						deferredTargets[memberID] = statePath
					} else {
						r.traceDeferredDispatchAtState(instance, event.Name, statePath)
					}
				} else if activeTargets == 1 {
					r.traceDeferredReplay(instance, event.Name)
				}
			}
		}
		dispatched, dispatchErr := r.dispatchGroup(r.ctx, event, groupID, true, nil)
		if dispatchErr != nil {
			r.trace = append(r.trace, anyMap{"type": "error", "code": dispatchErr.code})
			r.stableLabel = ""
			r.recordError(dispatchErr)
			break
		}
		if err := r.waitFor(r.ctx, dispatched, "group_dispatch"); err != nil {
			return err
		}
		r.lastDispatchQueued = activeTargets > 0
		for _, memberID := range r.groupMembers[groupID] {
			if statePath, ok := deferredTargets[memberID]; ok {
				r.traceDeferredDispatchAtState(r.instances[memberID], event.Name, statePath)
			}
		}
	case "set":
		instance, err := r.instanceForStep(step)
		if err != nil {
			return err
		}
		attr, err := requireString(stepMap(step), "attribute")
		if err != nil {
			return err
		}
		value := normalizeJSONValue(step["value"])
		r.flushTimerScheduled(-1)
		id, err := instanceID(step)
		if err != nil {
			return err
		}
		if !r.started[id] {
			r.recordLifecycleError("operation requires a started HSM")
			break
		}
		shouldTrace := r.shouldTraceScriptSet(attr)
		if err := r.validateScriptSet(attr, value); err != nil {
			r.trace = append(r.trace, anyMap{"type": "set", "attribute": attr, "value": value})
			r.trace = append(r.trace, anyMap{"type": "error", "code": err.code})
			r.recordError(err)
			break
		}
		if shouldTrace {
			r.trace = append(r.trace, anyMap{"type": "set", "attribute": attr, "value": value})
		}
		if err := r.waitFor(r.ctx, hsm.Set(r.ctx, instance, attr, r.runtimeAttrValue(attr, value)), "set"); err != nil {
			return err
		}
	case "call":
		instance, err := r.instanceForStep(step)
		if err != nil {
			return err
		}
		operation, err := requireString(stepMap(step), "operation")
		if err != nil {
			return err
		}
		r.trace = append(r.trace, anyMap{"type": "call", "operation": operation})
		id, err := instanceID(step)
		if err != nil {
			return err
		}
		if !r.started[id] {
			r.recordLifecycleError("operation requires a started HSM")
			break
		}
		args := []any{}
		if _, ok := step["data"]; ok {
			args = append(args, normalizeJSONValue(step["data"]))
		}
		if _, err := r.call(r.ctx, instance, operation, args...); err != nil {
			if errors.Is(err, hsm.ErrMissingOperation) {
				err = &conformanceError{code: "operation_error", message: operation}
				r.trace = append(r.trace, anyMap{"type": "error", "code": "operation_error"})
			}
			r.recordError(err)
		}
		if err := r.waitFor(r.ctx, instance.Wait(), "call"); err != nil {
			return err
		}
	case "snapshot":
		if groupRaw, ok := step["group"]; ok {
			groupID, err := requireStringValue(groupRaw)
			if err != nil {
				return err
			}
			memberIDs, ok := r.groupMembers[groupID]
			if !ok {
				conformanceErr := &conformanceError{code: "runtime_error", message: "unknown group " + groupID}
				r.trace = append(r.trace, anyMap{"type": "error", "code": conformanceErr.code})
				r.recordError(conformanceErr)
				break
			}
			lifecycleFailed := false
			for _, memberID := range memberIDs {
				if !r.started[memberID] {
					conformanceErr := &conformanceError{code: "runtime_error", message: "operation requires a started HSM"}
					r.trace = append(r.trace, anyMap{"type": "error", "code": conformanceErr.code})
					r.recordError(conformanceErr)
					lifecycleFailed = true
					break
				}
			}
			if lifecycleFailed {
				break
			}
			r.snapshots[groupID] = r.groupSnapshot(groupID)
			r.trace = append(r.trace, anyMap{"type": "snapshot", "group": groupID})
			r.stableLabel = "group:" + groupID
			break
		}
		instance, err := r.instanceForStep(step)
		if err != nil {
			return err
		}
		instanceID, err := instanceID(step)
		if err != nil {
			return err
		}
		if !r.started[instanceID] {
			conformanceErr := &conformanceError{code: "runtime_error", message: "operation requires a started HSM"}
			r.trace = append(r.trace, anyMap{"type": "error", "code": conformanceErr.code})
			r.recordError(conformanceErr)
			break
		}
		id, _ := step["id"].(string)
		if id == "" {
			id = "last"
		}
		if containsString(r.caseData.Features, "timer") {
			r.settle()
		}
		r.flushTimerScheduled(-1)
		snapshots := hsm.TakeSnapshots(r.ctx, instance)
		if len(snapshots) == 0 {
			return fmt.Errorf("snapshot unavailable for %s", instanceID)
		}
		snapshot := snapshots[0]
		r.snapshots[id] = r.normalizeSnapshot(snapshot)
		r.trace = append(r.trace, anyMap{"type": "snapshot", "state": snapshot.State})
		r.stableLabel = ""
	case "restart":
		instance, err := r.instanceForStep(step)
		if err != nil {
			return err
		}
		id, err := instanceID(step)
		if err != nil {
			return err
		}
		if !r.started[id] {
			r.recordLifecycleError("operation requires a started HSM")
			break
		}
		data := r.startData[id]
		if value, ok := step["data"]; ok {
			data = normalizeJSONValue(value)
		}
		r.flushTimerScheduled(-1)
		r.traceLifecycle(step, "restart")
		r.traceTimerCancelled()
		if err := r.waitFor(r.ctx, hsm.Restart(r.ctx, instance, data), "restart"); err != nil {
			return err
		}
		r.clearDeferredEventsForInstance(id)
		r.started[id] = true
		r.ever[id] = true
	case "stop":
		instance, err := r.instanceForStep(step)
		if err != nil {
			return err
		}
		id, err := instanceID(step)
		if err != nil {
			return err
		}
		if !r.started[id] {
			break
		}
		r.flushTimerScheduled(-1)
		r.traceLifecycle(step, "stop")
		if err := r.waitFor(r.ctx, hsm.Stop(r.ctx, instance), "stop"); err != nil {
			return err
		}
		r.traceTimerCancelled()
		r.clearDeferredEventsForInstance(id)
		r.started[id] = false
	case "sleep":
		duration, err := requireDurationMillis(step, "millis")
		if err != nil {
			return err
		}
		time.Sleep(duration)
	case "tick":
		duration, err := requireDurationMillis(step, "millis")
		if err != nil {
			return err
		}
		r.flushTimerScheduled(-1)
		r.clock.Advance(duration, r.deliverLogicalTimer)
		r.settle()
		r.flushTimerScheduled(-1)
	case "expect":
		return r.assertExpectationObject(object(step["expect"]))
	default:
		return fmt.Errorf("unsupported script op %v", step["op"])
	}
	r.settle()
	return nil
}

func (r *runner) settle() {
	settled := false
	minTurns := 1
	if containsString(r.caseData.Features, "timer") {
		minTurns = 5
	}
	for i := 0; i < 10; i++ {
		r.drainPendingWork()
		for _, id := range r.instanceOrder {
			if !r.started[id] {
				continue
			}
			instance := r.instances[id]
			if instance == nil {
				continue
			}
			if err := r.waitFor(r.ctx, instance.Wait(), "settle "+id); err != nil {
				r.recordError(err)
				return
			}
		}
		runtimeYield()
		if err := r.waitSchedulerTurn(r.ctx, "settle scheduler turn"); err != nil {
			r.recordError(err)
			return
		}
		if !r.hasPendingWork() && i+1 >= minTurns {
			settled = true
			break
		}
	}
	if !settled && r.hasPendingWork() {
		r.recordError(conformanceError{code: "runtime_not_settled", message: "pending runner work remained after settle turns"})
		return
	}
	r.recordPendingTimerKindError()
}

func (r *runner) hasPendingWork() bool {
	r.pendingWorkMu.Lock()
	defer r.pendingWorkMu.Unlock()
	return len(r.pendingWork) > 0
}

func (r *runner) recordPendingTimerKindError() {
	r.timerMu.Lock()
	pending := len(r.pendingTimerKinds)
	if pending == 0 {
		r.timerMu.Unlock()
		return
	}
	r.pendingTimerKinds = nil
	r.pendingTimerKindsByG = map[uint64][]string{}
	r.timerMu.Unlock()
	r.recordError(conformanceError{code: "timer_binding_error", message: fmt.Sprintf("%d timer source kind token(s) were not consumed by clock timer creation", pending)})
}

func (r *runner) drainPendingWork() {
	for {
		r.pendingWorkMu.Lock()
		if len(r.pendingWork) == 0 {
			r.pendingWorkMu.Unlock()
			return
		}
		work := r.pendingWork
		r.pendingWork = nil
		r.pendingWorkMu.Unlock()
		for _, done := range work {
			select {
			case err := <-done:
				if err != nil {
					r.recordError(err)
				}
			case <-r.ctx.Done():
				return
			}
		}
	}
}

func (i *confInstance) Wait() <-chan struct{} {
	return hsm.AfterProcess(i.Context(), i)
}

func (r *runner) deliverLogicalTimer(eventName string, trigger func()) {
	if trigger == nil {
		return
	}
	waiters := make([]<-chan struct{}, 0, len(r.instanceOrder))
	for _, id := range r.instanceOrder {
		if !r.started[id] {
			continue
		}
		instance := r.instances[id]
		if instance == nil {
			continue
		}
		waiters = append(waiters, hsm.AfterProcess(r.ctx, instance, hsm.Event{Name: eventName}))
	}
	trigger()
	if len(waiters) == 0 {
		runtimeYield()
		return
	}
	if err := r.waitForAny(r.ctx, waiters, "timer "+eventName); err != nil {
		r.recordError(err)
	}
}

func (r *runner) waitFor(ctx context.Context, done any, label string) error {
	if ctx == nil {
		ctx = r.ctx
	}
	if ctx == nil {
		return conformanceError{code: "runtime_wait_without_context", message: label}
	}
	switch done := done.(type) {
	case hsm.Completion:
		if done == nil {
			return nil
		}
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return conformanceError{code: "runtime_wait_cancelled", message: label}
		}
	case <-chan struct{}:
		if done == nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return conformanceError{code: "runtime_wait_cancelled", message: label}
		}
	case chan struct{}:
		if done == nil {
			return nil
		}
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return conformanceError{code: "runtime_wait_cancelled", message: label}
		}
	default:
		return conformanceError{code: "runtime_wait_invalid_waiter", message: label}
	}
}

func (r *runner) waitForAny(ctx context.Context, waiters []<-chan struct{}, label string) error {
	if len(waiters) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = r.ctx
	}
	if ctx == nil {
		return conformanceError{code: "runtime_wait_without_context", message: label}
	}
	cases := make([]reflect.SelectCase, 0, len(waiters)+1)
	for _, waiter := range waiters {
		if waiter == nil {
			continue
		}
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(waiter)})
	}
	if len(cases) == 0 {
		return nil
	}
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
	chosen, _, _ := reflect.Select(cases)
	if chosen == len(cases)-1 {
		return conformanceError{code: "runtime_wait_cancelled", message: label}
	}
	return nil
}

func (r *runner) waitSchedulerTurn(ctx context.Context, label string) error {
	if ctx == nil {
		ctx = r.ctx
	}
	if ctx == nil {
		return conformanceError{code: "runtime_wait_without_context", message: label}
	}
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return conformanceError{code: "runtime_wait_cancelled", message: label}
	}
}

func (r *runner) executeBehavior(ctx context.Context, sm *confInstance, event hsm.Event, behaviorID, role string) (any, error) {
	program := r.caseData.Behaviors[behaviorID]
	if len(program) == 0 {
		return nil, fmt.Errorf("missing behavior program %q", behaviorID)
	}
	event = r.applyEventMemory(sm, event)
	originalEventName := event.Name
	var result any
	for _, op := range program {
		value, err := r.executeBehaviorOp(ctx, sm, &event, op, behaviorID, role)
		r.rememberEvent(sm, originalEventName, event)
		signalActivityStarted(ctx)
		if err != nil {
			if errors.Is(err, errBehaviorCancelled) {
				return nil, nil
			}
			return nil, err
		}
		result = value
		if strings.HasPrefix(fmt.Sprint(op["op"]), "return_") && (role == "guard" || role == "operation" || role == "timer") {
			return result, nil
		}
	}
	return result, nil
}

func (r *runner) eventMemoryKey(sm *confInstance, eventName string) string {
	id := ""
	for instanceID, instance := range r.instances {
		if instance == sm {
			id = instanceID
			break
		}
	}
	if id == "" && sm != nil {
		id = sm.State()
	}
	return id + "\x00" + eventName
}

func (r *runner) applyEventMemory(sm *confInstance, event hsm.Event) hsm.Event {
	r.eventMemoryMu.Lock()
	defer r.eventMemoryMu.Unlock()
	if remembered, ok := r.eventMemory[r.eventMemoryKey(sm, event.Name)]; ok {
		return remembered
	}
	return event
}

func (r *runner) rememberEvent(sm *confInstance, originalName string, event hsm.Event) {
	if originalName == "" {
		originalName = event.Name
	}
	r.eventMemoryMu.Lock()
	defer r.eventMemoryMu.Unlock()
	r.eventMemory[r.eventMemoryKey(sm, originalName)] = event
}

func (r *runner) clearEventMemory(sm *confInstance, eventName string) {
	r.eventMemoryMu.Lock()
	defer r.eventMemoryMu.Unlock()
	delete(r.eventMemory, r.eventMemoryKey(sm, eventName))
}

func (r *runner) setEventMetadata(event *hsm.Event, name string, value any) {
	if event == nil {
		return
	}
	switch name {
	case "name", "id", "source", "target":
		return
	default:
		metadata := object(event.Schema)
		if metadata == nil {
			metadata = anyMap{}
		}
		metadata[name] = value
		event.Schema = metadata
	}
}

func (r *runner) executeBehaviorOp(ctx context.Context, sm *confInstance, event *hsm.Event, operation op, behaviorID, role string) (any, error) {
	switch operation["op"] {
	case "trace":
		if r.deferReplayBarrier {
			r.deferReplayBarrier = false
		} else {
			r.traceDeferredReplayFromBehavior(sm)
		}
		r.trace = append(r.trace, anyMap{"type": "trace", "value": operation["value"]})
	case "set_attr":
		name, _ := requireString(stepMap(operation), "name")
		name = r.scopedAttrName(ctx, name)
		value := normalizeJSONValue(operation["value"])
		if err := r.validateScriptSet(name, value); err != nil {
			r.trace = append(r.trace, anyMap{"type": "error", "code": err.code})
			return nil, err
		}
		set := hsm.Set(ctx, sm, name, r.runtimeAttrValue(name, value))
		if role == "operation" && !r.inRuntimeProcessing(ctx, sm) {
			if err := r.waitFor(ctx, set, "operation set_attr"); err != nil {
				return nil, err
			}
		} else if role == "activity" {
			r.addPendingWork(set)
		}
	case "set_attr_from_event_data":
		name, _ := requireString(stepMap(operation), "name")
		name = r.scopedAttrName(ctx, name)
		value := eventDataPath(*event, fmt.Sprint(operation["path"]))
		if err := r.validateScriptSet(name, value); err != nil {
			r.trace = append(r.trace, anyMap{"type": "error", "code": err.code})
			return nil, err
		}
		set := hsm.Set(ctx, sm, name, r.runtimeAttrValue(name, value))
		if role == "operation" && !r.inRuntimeProcessing(ctx, sm) {
			if err := r.waitFor(ctx, set, "operation set_attr_from_event_data"); err != nil {
				return nil, err
			}
		} else if role == "activity" {
			r.addPendingWork(set)
		}
	case "get_attr", "return_attr":
		name, _ := requireString(stepMap(operation), "name")
		name = r.scopedAttrName(ctx, name)
		value, _ := hsm.Get(ctx, sm, name)
		return value, nil
	case "return_value":
		return operation["value"], nil
	case "return_equals":
		name, _ := requireString(stepMap(operation), "name")
		name = r.scopedAttrName(ctx, name)
		value, _ := hsm.Get(ctx, sm, name)
		return reflect.DeepEqual(value, normalizeJSONValue(operation["value"])), nil
	case "event_name_equals":
		return event.Name == operation["value"], nil
	case "event_data_equals":
		return reflect.DeepEqual(eventDataPath(*event, fmt.Sprint(operation["path"])), normalizeJSONValue(operation["value"])), nil
	case "event_data_get":
		return eventDataPath(*event, fmt.Sprint(operation["path"])), nil
	case "event_application_metadata_equals":
		name, _ := requireString(stepMap(operation), "name")
		return reflect.DeepEqual(eventApplicationMetadata(*event, name), normalizeJSONValue(operation["value"])), nil
	case "event_metadata_set":
		name, _ := requireString(stepMap(operation), "name")
		r.setEventMetadata(event, name, normalizeJSONValue(operation["value"]))
	case "event_metadata_get":
		name, _ := requireString(stepMap(operation), "name")
		return eventMetadata(*event, name), nil
	case "event_metadata_equals":
		name, _ := requireString(stepMap(operation), "name")
		return reflect.DeepEqual(eventMetadata(*event, name), normalizeJSONValue(operation["value"])), nil
	case "dispatch":
		nested, err := eventFromValue(operation["event"])
		if err != nil {
			return nil, err
		}
		waitForDispatch := role == "operation" && !r.inRuntimeProcessing(ctx, sm)
		var dispatched hsm.Completion
		if target, ok := operation["target"].(string); ok && target == "all" {
			r.trace = append(r.trace, anyMap{"type": "dispatch", "event": nested.Name, "target": "all"})
			for _, id := range r.instanceOrder {
				if instance := r.instances[id]; instance != nil && r.started[id] {
					r.traceDeferredDispatch(instance, nested.Name)
				}
			}
			dispatched = r.dispatchAll(ctx, nested, false, sm)
		} else if target, ok := operation["target"].(string); ok {
			r.trace = append(r.trace, anyMap{"type": "dispatch", "event": nested.Name, "target": target})
			if instance := r.instances[target]; instance != nil && r.started[target] {
				r.traceDeferredDispatch(instance, nested.Name)
			}
			dispatched = r.dispatchTo(ctx, nested, []string{target}, false, sm)
		} else if target, ok := operation["instance"].(string); ok {
			r.trace = append(r.trace, anyMap{"type": "dispatch", "event": nested.Name, "target": target})
			if instance := r.instances[target]; instance != nil && r.started[target] {
				r.traceDeferredDispatch(instance, nested.Name)
			}
			dispatched = r.dispatchTo(ctx, nested, []string{target}, false, sm)
		} else if groupID, ok := operation["group"].(string); ok {
			r.trace = append(r.trace, anyMap{"type": "dispatch", "event": nested.Name, "target": groupID})
			group := r.groups[groupID]
			if group == nil {
				conformanceErr := conformanceError{code: "runtime_error", message: "group " + groupID}
				r.trace = append(r.trace, anyMap{"type": "error", "code": conformanceErr.code})
				r.stableLabel = ""
				return nil, conformanceErr
			}
			for _, memberID := range r.groupMembers[groupID] {
				if instance := r.instances[memberID]; instance != nil && r.started[memberID] {
					r.traceDeferredDispatch(instance, nested.Name)
				}
			}
			var dispatchErr *conformanceError
			dispatched, dispatchErr = r.dispatchGroup(ctx, nested, groupID, false, sm)
			if dispatchErr != nil {
				r.trace = append(r.trace, anyMap{"type": "error", "code": dispatchErr.code})
				r.stableLabel = ""
				return nil, dispatchErr
			}
		} else {
			r.trace = append(r.trace, anyMap{"type": "dispatch", "event": nested.Name})
			r.clearEventMemory(sm, nested.Name)
			dispatched = hsm.Dispatch(ctx, sm, nested)
		}
		if waitForDispatch {
			if err := r.waitFor(ctx, dispatched, "operation dispatch"); err != nil {
				return nil, err
			}
		} else {
			r.addPendingWork(dispatched)
		}
		if !waitForDispatch && role == "activity" {
			runtimeYield()
			if ctx.Err() != nil || r.activityEventExits(ctx, nested.Name) {
				return nil, errBehaviorCancelled
			}
		}
	case "call":
		name, _ := requireString(stepMap(operation), "name")
		if _, err := r.call(ctx, sm, name); err != nil {
			if errors.Is(err, hsm.ErrMissingOperation) {
				conformanceErr := conformanceError{code: "operation_error", message: name}
				r.trace = append(r.trace, anyMap{"type": "error", "code": conformanceErr.code})
				return nil, conformanceErr
			}
			return nil, err
		}
		if r.traceContractIncludes("call") && behaviorCallOpIsTraceable(role) {
			r.trace = append(r.trace, anyMap{"type": "call", "operation": name})
		}
		if role == "activity" {
			select {
			case <-sm.Wait():
			case <-ctx.Done():
				return nil, errBehaviorCancelled
			}
			if ctx.Err() != nil || r.activityEventExits(ctx, r.operationNameInScope(ctx, sm, name)) {
				return nil, errBehaviorCancelled
			}
		}
	case "snapshot":
		snapshot := hsm.TakeSnapshot(ctx, sm)
		state := snapshot.State
		if currentBehaviorState := behaviorState(ctx); currentBehaviorState != "" && (role == "entry" || role == "exit" || role == "activity") {
			state = currentBehaviorState
			if role == "entry" {
				state = path.Dir(state)
			}
		} else if role == "timer" {
			if scope := behaviorScope(ctx, ""); scope != "" {
				state = rootPath(scope)
			}
		}
		r.trace = append(r.trace, anyMap{"type": "snapshot", "state": state})
		return r.normalizeSnapshot(snapshot), nil
	case "raise":
		if code, ok := operation["code"].(string); ok {
			r.trace = append(r.trace, anyMap{"type": "error", "code": code})
			return nil, conformanceError{code: code, message: fmt.Sprint(operation["value"])}
		}
		raised, err := eventFromValue(operation["event"])
		if err != nil {
			return nil, err
		}
		r.trace = append(r.trace, anyMap{"type": "raise", "event": raised.Name})
		if statePath := behaviorState(ctx); statePath != "" {
			r.traceDeferredDispatchAtState(sm, raised.Name, statePath)
		} else {
			r.traceDeferredDispatch(sm, raised.Name)
		}
		r.clearEventMemory(sm, raised.Name)
		dispatched := hsm.Dispatch(ctx, sm, raised)
		waitForRaise := role == "operation" && !r.inRuntimeProcessing(ctx, sm)
		if waitForRaise {
			if err := r.waitFor(ctx, dispatched, "operation raise"); err != nil {
				return nil, err
			}
		} else {
			r.addPendingWork(dispatched)
		}
		if !waitForRaise && role == "activity" {
			runtimeYield()
			if ctx.Err() != nil || r.activityEventExits(ctx, raised.Name) {
				return nil, errBehaviorCancelled
			}
		}
	case "sleep":
		if role == "activity" {
			timer := time.NewTimer(durationMillis(operation["millis"]))
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				r.recordActivityCancel(ctx, behaviorID)
				return nil, errBehaviorCancelled
			}
			break
		}
		time.Sleep(durationMillis(operation["millis"]))
	case "yield":
		if role == "activity" && ctx.Err() != nil {
			r.recordActivityCancel(ctx, behaviorID)
			return nil, errBehaviorCancelled
		}
		runtimeYield()
	default:
		return nil, fmt.Errorf("unsupported behavior op %v", operation["op"])
	}
	return nil, nil
}

func runtimeYield() {
	time.Sleep(0)
}

func signalActivityStarted(ctx context.Context) {
	started, _ := ctx.Value(activityStartedKey{}).(chan struct{})
	if started == nil {
		return
	}
	select {
	case <-started:
	default:
		close(started)
	}
}

func activityStateExited(ctx context.Context, sm *confInstance) bool {
	state := behaviorState(ctx)
	if state == "" || sm == nil {
		return false
	}
	current := sm.State()
	return current != state && !hsm.IsAncestor(state, current)
}

func (r *runner) activityEventExits(ctx context.Context, eventName string) bool {
	state := behaviorState(ctx)
	if state == "" || eventName == "" {
		return false
	}
	return walkStateAncestors(state, func(current string) bool {
		return r.activityExitEvents[current][eventName]
	}) != ""
}

func (r *runner) recordError(err error) {
	if err == nil || r.lastError != nil {
		return
	}
	var conformanceErrPtr *conformanceError
	if errors.As(err, &conformanceErrPtr) {
		r.lastError = conformanceErrPtr
		return
	}
	var conformanceErr conformanceError
	if errors.As(err, &conformanceErr) {
		r.lastError = &conformanceErr
		return
	}
	r.lastError = &conformanceError{message: err.Error()}
}

func (r *runner) recordExpectedError(err error) {
	r.recordError(err)
	expected := object(r.caseData.Expect["error"])
	if expected == nil {
		return
	}
	if len(r.trace) > 0 {
		if lastType, _ := r.trace[len(r.trace)-1]["type"].(string); lastType == "error" {
			return
		}
	}
	code, _ := expected["code"].(string)
	if code == "" {
		code = "runtime_error"
	}
	r.trace = append(r.trace, anyMap{"type": "error", "code": code})
}

func (r *runner) recordLifecycleError(message string) {
	// Lifecycle IR errors are adapter-normalized so channel-returning Go APIs
	// can keep inactive Set/Restart/Dispatch as closed no-ops while Call
	// exposes ErrInvalidState directly.
	err := &conformanceError{code: "runtime_error", message: message}
	r.trace = append(r.trace, anyMap{"type": "error", "code": err.code})
	r.recordError(err)
}

func (r *runner) clearDeferredEventsForInstance(instanceID string) {
	if instanceID == "" {
		return
	}
	filtered := r.pendingDeferred[:0]
	for _, deferred := range r.pendingDeferred {
		if deferred.instanceID != instanceID {
			filtered = append(filtered, deferred)
		}
	}
	r.pendingDeferred = filtered
}

func (r *runner) recordActivityCancel(ctx context.Context, behaviorID string) {
	if recorder, _ := ctx.Value(activityCancelRecorderKey{}).(*activityCancelRecorder); recorder != nil {
		recorder.mark(behaviorID)
		return
	}
	r.appendActivityCancel(behaviorID)
}

func (r *runner) appendActivityCancel(behaviorID string) {
	if r.cancelledActivities[behaviorID] {
		return
	}
	r.cancelledActivities[behaviorID] = true
	r.trace = append(r.trace, anyMap{"type": "activity_cancel", "behavior": behaviorID})
}

func (r *runner) traceLifecycle(step op, kind string) {
	if trace, ok := step["trace"].(bool); ok && !trace {
		return
	}
	if !r.traceContractIncludes(kind) && !((kind == "restart" || kind == "stop") && containsString(r.caseData.Features, "activity") && containsString(r.caseData.Features, "cancellation")) {
		return
	}
	event := anyMap{"type": kind}
	r.trace = append(r.trace, event)
}

func (r *runner) traceTimerCancelled() {
	if r.traceContractIncludes("timer_cancelled") {
		r.trace = append(r.trace, anyMap{"type": "timer_cancelled"})
	}
}

func (r *runner) traceContractIncludes(kind string) bool {
	for _, item := range arrayAny(r.caseData.Expect["trace"]) {
		entry := object(item)
		if entry == nil {
			continue
		}
		if entryType, _ := entry["type"].(string); entryType == kind {
			return true
		}
	}
	return false
}

func (r *runner) instanceForStep(step op) (*confInstance, error) {
	id, err := instanceID(step)
	if err != nil {
		return nil, err
	}
	if instance := r.instances[id]; instance != nil {
		return instance, nil
	}
	return nil, fmt.Errorf("unknown instance %q", id)
}

func instanceID(step op) (string, error) {
	for _, key := range []string{"instance", "target"} {
		if id, ok := step[key].(string); ok && id != "" {
			return id, nil
		}
	}
	return "default", nil
}

func (r *runner) stepTargets(step op) ([]string, error) {
	if value, ok := step["targets"]; ok {
		raw := arrayAny(value)
		if len(raw) == 0 {
			return nil, fmt.Errorf("dispatch_to requires non-empty targets")
		}
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			id, err := memberIDValue(item)
			if err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, nil
	}
	id, err := instanceID(step)
	if err != nil {
		return nil, err
	}
	if id == "default" {
		if _, hasInstance := step["instance"]; !hasInstance {
			if _, hasTarget := step["target"]; !hasTarget {
				return nil, fmt.Errorf("dispatch_to requires instance, target, or targets")
			}
		}
	}
	return []string{id}, nil
}

func (r *runner) stableState() any {
	if r.stableLabel != "" {
		return r.stableLabel
	}
	if _, ok := r.instances["default"]; ok {
		return r.stateFor("default")
	}
	for _, id := range r.instanceOrder {
		return r.stateFor(id)
	}
	return r.stateFor("default")
}

func (r *runner) assertExpectations() error {
	if err := r.assertExpectationObject(r.caseData.Expect); err != nil {
		return err
	}
	return nil
}

func (r *runner) assertExpectationObject(expect anyMap) error {
	if queued, ok := expect["queued"].(bool); ok && r.lastDispatchQueued != queued {
		return fmt.Errorf("dispatch queued mismatch: got %t want %t", r.lastDispatchQueued, queued)
	}
	if expectedError := object(expect["error"]); expectedError != nil {
		if r.lastError == nil {
			return fmt.Errorf("expected error but none was recorded")
		}
		if code, ok := expectedError["code"].(string); ok && code != "" && r.lastError.code != code {
			return fmt.Errorf("error code mismatch: got %s want %s", r.lastError.code, code)
		}
		if contains, ok := expectedError["message_contains"].(string); ok && contains != "" && !strings.Contains(r.lastError.message, contains) {
			return fmt.Errorf("error message mismatch: got %q want containing %q", r.lastError.message, contains)
		}
	} else if r.lastError != nil {
		return fmt.Errorf("unexpected error: %s", r.lastError.Error())
	}
	if expected, ok := expect["state"].(string); ok {
		if actual := r.stateFor("default"); actual != expected {
			return fmt.Errorf("state mismatch: got %s want %s", actual, expected)
		}
	}
	if states := object(expect["states"]); states != nil {
		for id, expected := range states {
			actual := r.stateFor(id)
			if actual != expected {
				return fmt.Errorf("state mismatch for %s: got %s want %v", id, actual, expected)
			}
		}
	}
	if attrs := object(expect["attributes"]); attrs != nil {
		for name, expected := range attrs {
			value, _ := r.getExpectedAttribute("default", name)
			if !reflect.DeepEqual(normalizeJSONValue(value), normalizeJSONValue(expected)) {
				return fmt.Errorf("attribute %s mismatch: got %#v want %#v", name, value, expected)
			}
		}
	}
	if perInstanceAttrs := object(expect["instance_attributes"]); perInstanceAttrs != nil {
		for id, rawAttrs := range perInstanceAttrs {
			attrs := object(rawAttrs)
			if attrs == nil {
				return fmt.Errorf("instance_attributes for %s must be an object", id)
			}
			for name, expected := range attrs {
				value, _ := r.getExpectedAttribute(id, name)
				if !reflect.DeepEqual(normalizeJSONValue(value), normalizeJSONValue(expected)) {
					return fmt.Errorf("attribute %s for instance %s mismatch: got %#v want %#v", name, id, value, expected)
				}
			}
		}
	}
	if snapshots := object(expect["snapshots"]); snapshots != nil {
		if !valueContains(normalizeJSONValue(r.snapshots), normalizeJSONValue(snapshots)) {
			return fmt.Errorf("snapshots mismatch:\nactual: %s\nexpect: %s", mustJSON(r.snapshots), mustJSON(snapshots))
		}
	}
	if expectedTrace := arrayAny(expect["trace"]); expectedTrace != nil {
		if !reflect.DeepEqual(normalizeJSONValue(r.trace), normalizeJSONValue(expectedTrace)) {
			return fmt.Errorf("trace mismatch:\nactual: %s\nexpect: %s", mustJSON(r.trace), mustJSON(expectedTrace))
		}
	}
	return nil
}

func (r *runner) getExpectedAttribute(instanceID, name string) (any, bool) {
	instance := r.instances[instanceID]
	if instance == nil {
		return nil, false
	}
	if value, ok := hsm.Get(r.ctx, instance, name); ok {
		return value, true
	}
	if path.IsAbs(name) {
		return nil, false
	}
	state := instance.State()
	for state != "" && state != "." && state != "/" {
		if attrs := r.scopedAttrs[state]; containsString(attrs, name) {
			return hsm.Get(r.ctx, instance, path.Join(state, name))
		}
		next := path.Dir(state)
		if next == state {
			break
		}
		state = next
	}
	return nil, false
}

func (r *runner) stateFor(id string) string {
	instance := r.instances[id]
	if instance == nil {
		return ""
	}
	if !r.started[id] && !r.ever[id] {
		return ""
	}
	return instance.State()
}

func (r *runner) normalizeSnapshot(snapshot hsm.Snapshot) anyMap {
	attrs := anyMap{}
	basenameCounts := map[string]int{}
	type attrItem struct {
		name     string
		basename string
		value    any
	}
	items := []attrItem{}
	prefix := snapshot.QualifiedName + "/"
	for name, value := range snapshot.Attributes {
		normalizedName := name
		if strings.HasPrefix(normalizedName, prefix) {
			normalizedName = strings.TrimPrefix(normalizedName, prefix)
		}
		basename := path.Base(normalizedName)
		basenameCounts[basename]++
		items = append(items, attrItem{name: normalizedName, basename: basename, value: normalizeJSONValue(value)})
	}
	for _, item := range items {
		attrs[item.name] = item.value
		if basenameCounts[item.basename] == 1 {
			if _, exists := attrs[item.basename]; !exists {
				attrs[item.basename] = item.value
			}
		}
	}
	out := anyMap{
		"id":             snapshot.ID,
		"qualified_name": snapshot.QualifiedName,
		"state":          snapshot.State,
		"attributes":     attrs,
		"queue_len":      snapshot.QueueLen,
	}
	transitions := r.normalizedSnapshotTransitions(snapshot)
	if len(transitions) > 0 {
		out["transitions"] = transitions
	}
	events := r.normalizedSnapshotEvents(snapshot)
	if len(events) > 0 {
		out["events"] = events
	}
	return out
}

func (r *runner) normalizedSnapshotTransitions(snapshot hsm.Snapshot) []any {
	transitions := make([]any, 0, len(snapshot.Transitions))
	for _, transition := range snapshot.Transitions {
		target := any(transition.Target)
		if transition.Target == "" {
			target = nil
		}
		events := append([]string(nil), transition.Events...)
		for i, event := range events {
			if normalized, ok := r.canonicalTimerEventName(event); ok {
				events[i] = normalized
			}
		}
		transitions = append(transitions, anyMap{
			"name":   transition.Name,
			"kind":   normalizeTransitionKind(transition.Kind),
			"source": transition.Source,
			"target": target,
			"events": events,
			"guard":  transition.Guard,
		})
	}
	return transitions
}

func (r *runner) normalizedSnapshotEvents(snapshot hsm.Snapshot) []any {
	events := []any{}
	if len(snapshot.Events) == 0 {
		events = r.snapshotEventsFromStateIR(snapshot.State, snapshot.Events)
	} else {
		r.runtimeTimerEventNamesByOwner(snapshot.State, snapshot.Events)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[snapshotEventDetailKey(object(event))] = true
	}
	for _, event := range snapshot.Events {
		name := event.Name
		if event.Kind == hsm.TimeEventKind {
			if normalized, ok := r.canonicalTimerEventName(event.Name); ok {
				name = normalized
			}
		}
		var target any = event.Target
		if event.Target == "" {
			target = nil
		}
		guard := event.Guard
		if r.snapshotEventIsPlainWhen(snapshot.State, event.Name) {
			guard = false
		}
		detail := anyMap{
			"name":   name,
			"kind":   normalizeEventKind(event.Kind),
			"target": target,
			"guard":  guard,
			"schema": normalizeEventSchema(event.Kind, event.Schema),
		}
		key := snapshotEventDetailKey(detail)
		if seen[key] {
			continue
		}
		events = append(events, detail)
		seen[key] = true
	}
	return events
}

func snapshotEventDetailKey(event anyMap) string {
	return fmt.Sprintf("%v|%v|%v|%v|%v", event["name"], event["kind"], event["target"], event["guard"], event["schema"])
}

func (r *runner) snapshotEventsFromStateIR(statePath string, runtimeEvents []hsm.EventSnapshot) []any {
	timerNames := r.runtimeTimerEventNamesByOwner(statePath, runtimeEvents)
	events := []any{}
	seen := map[string]bool{}
	appendOwner := func(owner string) {
		for _, event := range r.snapshotEventsForOwner(owner, timerNames) {
			key := snapshotEventDetailKey(object(event))
			if seen[key] {
				continue
			}
			seen[key] = true
			events = append(events, event)
		}
	}
	for _, owner := range stateOwnerChain(statePath) {
		appendOwner(owner)
	}
	return events
}

func (r *runner) runtimeTimerEventNamesByOwner(statePath string, events []hsm.EventSnapshot) map[string][]string {
	names := map[string][]string{}
	owners := r.snapshotOwnerChain(statePath)
	expected := make([]timerEventDef, 0)
	expectedByName := map[string]timerEventDef{}
	for _, owner := range owners {
		for _, def := range r.timerEventsByOwner[owner] {
			expected = append(expected, def)
			expectedByName[def.name] = def
		}
	}
	timerEvents := make([]hsm.EventSnapshot, 0)
	for _, event := range events {
		if event.Kind != hsm.TimeEventKind {
			continue
		}
		timerEvents = append(timerEvents, event)
	}
	if len(timerEvents) > len(expected) {
		r.recordError(conformanceError{
			code:    "timer_snapshot_binding_error",
			message: fmt.Sprintf("snapshot has %d runtime timer event(s), active IR has %d timer event(s)", len(timerEvents), len(expected)),
		})
		return names
	}
	used := map[string]bool{}
	for index, event := range timerEvents {
		name := ""
		if mapped := r.timerEventNameForRuntimeEvent(event.Name); mapped != "" {
			if _, exists := expectedByName[mapped]; !exists {
				r.recordError(conformanceError{code: "timer_snapshot_binding_error", message: "snapshot timer event is bound outside active IR timer set: " + mapped})
				return names
			}
			r.bindTimerEventName(event.Name, mapped)
			name = mapped
		} else if bound, ok := r.canonicalTimerEventName(event.Name); ok {
			if _, exists := expectedByName[bound]; !exists {
				r.recordError(conformanceError{code: "timer_snapshot_binding_error", message: "snapshot timer event is bound outside active IR timer set: " + bound})
				return names
			}
			name = bound
		} else {
			name = expected[index].name
			r.bindTimerEventName(event.Name, name)
		}
		if used[name] {
			r.recordError(conformanceError{code: "timer_snapshot_binding_error", message: "duplicate snapshot timer binding: " + name})
			return names
		}
		used[name] = true
		owner := path.Dir(path.Dir(name))
		names[owner] = append(names[owner], name)
	}
	return names
}

func (r *runner) snapshotOwnerChain(statePath string) []string {
	return stateOwnerChain(statePath)
}

func (r *runner) snapshotEventsForOwner(statePath string, runtimeTimerNames map[string][]string) []any {
	stateIR := r.stateIRForPath(statePath)
	if stateIR == nil {
		return nil
	}
	events := []any{}
	transitionOrdinal := 0
	timerOrdinal := 0
	for _, transitionAny := range arrayAny(stateIR["transitions"]) {
		transition := object(transitionAny)
		if transition == nil {
			continue
		}
		transitionOrdinal++
		trigger := object(transition["trigger"])
		if trigger == nil {
			if on, ok := transition["on"]; ok {
				trigger = anyMap{"kind": "on", "event": on}
			}
		}
		if trigger == nil {
			continue
		}
		target, _ := transition["target"].(string)
		var normalizedTarget any
		if target != "" {
			sourceRoot, targetRoot := r.snapshotRoots(statePath)
			normalizedTarget = resolveTransitionTarget(target, statePath, "", false, sourceRoot, targetRoot)
		}
		guard := transition["guard"] != nil
		kindName, _ := trigger["kind"].(string)
		switch kindName {
		case "on", "on_set", "on_call", "when":
			_, targetRoot := r.snapshotRoots(statePath)
			names, err := r.transitionEventNames(transition, targetRoot)
			if err != nil {
				continue
			}
			eventKind := 281
			schema := any(nil)
			switch kindName {
			case "on_set", "when":
				eventKind = 71965
				schema = "AttributeChange"
			case "on_call":
				eventKind = 71966
				schema = "CallData"
			}
			if kindName == "when" && transition["guard"] == nil {
				guard = false
			}
			for _, name := range names {
				events = append(events, anyMap{
					"name":   name,
					"kind":   eventKind,
					"target": normalizedTarget,
					"guard":  guard,
					"schema": schema,
				})
			}
		case "after", "every", "at":
			timerOrdinal++
			name := ""
			if names := runtimeTimerNames[statePath]; timerOrdinal > 0 && timerOrdinal <= len(names) {
				name = names[timerOrdinal-1]
			}
			if name == "" {
				name = r.timerEventNameForIR(statePath, kindName, transitionOrdinal)
			}
			if name == "" {
				continue
			}
			events = append(events, anyMap{
				"name":   name,
				"kind":   71964,
				"target": normalizedTarget,
				"guard":  guard,
				"schema": nil,
			})
		}
	}
	return events
}

func (r *runner) snapshotRoots(ownerPath string) (string, string) {
	root := "/" + rootName(ownerPath)
	best := ""
	for boundary := range r.submachineStates {
		if ownerPath != boundary && strings.HasPrefix(ownerPath, boundary+"/") {
			if len(boundary) > len(best) {
				best = boundary
			}
		}
	}
	if best != "" {
		if machineName := r.submachineModels[best]; machineName != "" {
			return "/" + machineName, best
		}
		return best, best
	}
	return root, root
}

func (r *runner) snapshotEventIsPlainWhen(statePath, eventName string) bool {
	stateIR := r.stateIRForPath(statePath)
	if stateIR == nil {
		return false
	}
	for _, transitionAny := range arrayAny(stateIR["transitions"]) {
		transition := object(transitionAny)
		trigger := object(transition["trigger"])
		if trigger == nil || trigger["kind"] != "when" || transition["guard"] != nil {
			continue
		}
		_, targetRoot := r.snapshotRoots(statePath)
		if r.transitionHandlesEvent(transition, targetRoot, eventName) {
			return true
		}
	}
	return false
}

func (r *runner) groupSnapshot(groupID string) anyMap {
	members := anyMap{}
	snapshots := hsm.TakeSnapshots(r.ctx, r.groups[groupID])
	for index, memberID := range r.groupMembers[groupID] {
		if index >= len(snapshots) {
			members[memberID] = ""
			continue
		}
		members[memberID] = snapshots[index].State
	}
	return anyMap{"members": members}
}

func normalizeEventKind(eventKind uint64) int {
	switch eventKind {
	case uint64(hsm.EventKind):
		return 281
	case uint64(hsm.CompletionEventKind):
		return 71962
	case uint64(hsm.ChangeEventKind):
		return 71965
	case uint64(hsm.CallEventKind):
		return 71966
	case uint64(hsm.TimeEventKind):
		return 71964
	default:
		return int(eventKind)
	}
}

func normalizeTransitionKind(transitionKind uint64) int {
	switch transitionKind {
	case uint64(hsm.ExternalKind):
		return 67343
	case uint64(hsm.SelfKind):
		return 67344
	case uint64(hsm.InternalKind):
		return 67345
	case uint64(hsm.LocalKind):
		return 67346
	case uint64(hsm.TransitionKind):
		return 263
	default:
		return int(transitionKind)
	}
}

func normalizeEventSchema(eventKind uint64, schema any) any {
	if eventKind == uint64(hsm.ChangeEventKind) && schema == nil {
		return "AttributeChange"
	}
	if eventKind == uint64(hsm.CallEventKind) && schema == nil {
		return "CallData"
	}
	if schema == nil {
		return nil
	}
	if reflect.TypeOf(schema) == reflect.TypeFor[hsm.AttributeChange]() {
		return "AttributeChange"
	}
	if reflect.TypeOf(schema) == reflect.TypeFor[hsm.CallData]() {
		return "CallData"
	}
	return normalizeJSONValue(schema)
}

func eventFromValue(raw any) (hsm.Event, error) {
	if name, ok := raw.(string); ok {
		return hsm.Event{Name: name}, nil
	}
	m := object(raw)
	if m == nil {
		return hsm.Event{}, fmt.Errorf("event must be a string or object")
	}
	name, err := requireString(m, "name")
	if err != nil {
		return hsm.Event{}, err
	}
	event := hsm.Event{Name: name, Data: normalizeJSONValue(m["data"]), Schema: normalizeJSONValue(m["metadata"])}
	if id, ok := m["id"].(string); ok {
		event.ID = id
	}
	if source, ok := m["source"].(string); ok {
		event.Source = source
	}
	if target, ok := m["target"].(string); ok {
		event.Target = target
	}
	return event, nil
}

func behaviorID(raw any) (string, error) {
	if s, ok := raw.(string); ok {
		return s, nil
	}
	m := object(raw)
	if m == nil {
		return "", fmt.Errorf("behavior ref must be string or object")
	}
	return requireString(m, "behavior")
}

func (r *runner) requireBehaviorID(raw any) (string, error) {
	id, err := behaviorID(raw)
	if err != nil {
		return "", err
	}
	if _, ok := r.caseData.Behaviors[id]; !ok {
		return "", fmt.Errorf("missing_behavior: %s", id)
	}
	return id, nil
}

func (r *runner) requireBehaviorIDs(raw []any) ([]string, error) {
	ids := make([]string, 0, len(raw))
	for _, ref := range raw {
		behaviorID, err := r.requireBehaviorID(ref)
		if err != nil {
			return nil, err
		}
		ids = append(ids, behaviorID)
	}
	return ids, nil
}

func eventName(raw any) string {
	name, _ := eventNameValue(raw)
	return name
}

func eventNameValue(raw any) (string, error) {
	if s, ok := raw.(string); ok {
		if s == "" {
			return "", fmt.Errorf("event reference requires non-empty string")
		}
		return s, nil
	}
	if m := object(raw); m != nil {
		name, err := requireString(m, "name")
		if err != nil {
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("event reference requires string or object event")
}

func eventForOnName(name string) hsm.Event {
	switch name {
	case hsm.InitialEvent.Name:
		return hsm.InitialEvent
	case hsm.ErrorEvent.Name:
		return hsm.ErrorEvent
	case hsm.AnyEvent.Name:
		return hsm.AnyEvent
	case hsm.FinalEvent.Name:
		return hsm.FinalEvent
	case hsm.ObservationEvent.Name:
		return hsm.ObservationEvent
	default:
		return hsm.Event{Name: name, Kind: hsm.EventKind}
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func walkStateAncestors(state string, visit func(string) bool) string {
	current := state
	for {
		if current == "" || current == "." || current == "/" {
			return ""
		}
		if visit != nil && visit(current) {
			return current
		}
		next := path.Dir(current)
		if next == current {
			return ""
		}
		current = next
	}
}

func stateOwnerChain(state string) []string {
	owners := []string{}
	walkStateAncestors(state, func(owner string) bool {
		owners = append(owners, owner)
		return false
	})
	return owners
}

func memberIDValue(raw any) (string, error) {
	if s, ok := raw.(string); ok && s != "" {
		return s, nil
	}
	return "", fmt.Errorf("group member must be a non-empty string")
}

func transitionTriggerKind(transitionIR anyMap) string {
	if trigger := object(transitionIR["trigger"]); trigger != nil {
		kindName, _ := trigger["kind"].(string)
		return kindName
	}
	if _, ok := transitionIR["on"]; ok {
		return "on"
	}
	return ""
}

func transitionKind(name string) uint64 {
	switch name {
	case "internal":
		return hsm.InternalKind
	case "local":
		return hsm.LocalKind
	case "external":
		return hsm.ExternalKind
	case "self":
		return hsm.SelfKind
	default:
		return hsm.NullKind
	}
}

func transitionKindOverride(kindValue uint64) hsm.RedefinableElement {
	return hsm.TransitionType(kindValue)
}

func validateOnlyKeys(m anyMap, allowed ...string) error {
	allowedSet := map[string]bool{}
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key := range m {
		if !allowedSet[key] {
			return fmt.Errorf("unexpected key %q", key)
		}
	}
	return nil
}

func resolveInitialTarget(target, ownerPath, sourceRoot, targetRoot string) string {
	return resolvePathInScope(target, ownerPath, true, sourceRoot, targetRoot)
}

func buildPathExpression(raw, resolved, sourceRoot, targetRoot string) string {
	if raw == "" {
		return resolved
	}
	clean := path.Clean(resolved)
	expressionRoot := sourceRoot
	if targetRoot != "" {
		expressionRoot = rootPath(targetRoot)
	}
	if expressionRoot == "" || sourceRoot == "" {
		return resolved
	}
	if clean == expressionRoot {
		return "."
	}
	if strings.HasPrefix(clean, expressionRoot+"/") {
		return "/" + strings.TrimPrefix(clean, expressionRoot+"/")
	}
	if sourceRoot != targetRoot {
		return resolved
	}
	return resolved
}

func resolveTransitionTarget(target, ownerPath, sourcePath string, bareTargets bool, sourceRoot, targetRoot string) string {
	if target == "." && sourcePath != "" {
		return sourcePath
	}
	return resolvePathInScope(target, ownerPath, bareTargets, sourceRoot, targetRoot)
}

func resolvePathInScope(raw, ownerPath string, bareRelativeToOwner bool, sourceRoot, targetRoot string) string {
	if raw == "" {
		return raw
	}
	if strings.HasPrefix(raw, "/") && sourceRoot != "" && targetRoot != "" && sourceRoot != targetRoot {
		clean := path.Clean(raw)
		if clean == sourceRoot {
			return targetRoot
		}
		if strings.HasPrefix(clean, sourceRoot+"/") {
			return path.Clean(targetRoot + strings.TrimPrefix(clean, sourceRoot))
		}
	}
	if !strings.HasPrefix(raw, "/") && !bareRelativeToOwner && sourceRoot != "" && targetRoot != "" && sourceRoot != targetRoot && raw != "." && !strings.HasPrefix(raw, "./") && !strings.HasPrefix(raw, "../") {
		return path.Clean(path.Join(targetRoot, raw))
	}
	return resolvePath(raw, ownerPath, bareRelativeToOwner)
}

func resolvePath(raw, ownerPath string, bareRelativeToOwner bool) string {
	if raw == "" {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		return path.Clean(raw)
	}
	if bareRelativeToOwner || raw == "." || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") {
		return path.Clean(path.Join(ownerPath, raw))
	}
	root := strings.Split(strings.Trim(ownerPath, "/"), "/")[0]
	return path.Clean(path.Join("/", root, raw))
}

func qualify(modelName, name string) string {
	if strings.HasPrefix(name, "/") {
		return path.Clean(name)
	}
	return path.Join("/", modelName, name)
}

func isRootPath(value string) bool {
	return strings.Count(strings.Trim(value, "/"), "/") == 0
}

func rootName(ownerPath string) string {
	trimmed := strings.Trim(ownerPath, "/")
	if trimmed == "" {
		return ""
	}
	return strings.Split(trimmed, "/")[0]
}

func rootPath(ownerPath string) string {
	name := rootName(ownerPath)
	if name == "" {
		return ""
	}
	return "/" + name
}

func localBuilderName(name string) string {
	if name == "" || !path.IsAbs(name) {
		return name
	}
	return path.Base(name)
}

func cloneMap(in anyMap) anyMap {
	out := anyMap{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func readPath(value any, dotted string) any {
	if dotted == "" {
		return value
	}
	current := value
	for _, part := range strings.Split(dotted, ".") {
		m := object(current)
		if m == nil {
			return nil
		}
		current = m[part]
	}
	return current
}

func eventDataPath(event hsm.Event, dotted string) any {
	if call, ok := event.Data.(hsm.CallData); ok {
		if len(call.Args) == 1 {
			return readPath(call.Args[0], dotted)
		}
		return readPath(call.Args, dotted)
	}
	if change, ok := event.Data.(hsm.AttributeChange); ok && dotted == "" {
		return unwrapDynamicAnyValue(change.New)
	}
	return readPath(unwrapDynamicAnyValue(event.Data), dotted)
}

func unwrapDynamicAnyValue(value any) any {
	if wrapped, ok := value.(dynamicAnyValue); ok {
		return wrapped.Value
	}
	return value
}

func behaviorCallOpIsTraceable(role string) bool {
	return role == "entry" || role == "exit" || role == "activity"
}

func eventMetadata(event hsm.Event, name string) any {
	switch name {
	case "name":
		return event.Name
	case "id":
		return event.ID
	case "source":
		return event.Source
	case "target":
		return event.Target
	default:
		return object(event.Schema)[name]
	}
}

func eventApplicationMetadata(event hsm.Event, name string) any {
	return object(event.Schema)[name]
}

func object(value any) anyMap {
	switch typed := value.(type) {
	case nil:
		return nil
	case anyMap:
		return typed
	case map[string]any:
		return anyMap(typed)
	default:
		return nil
	}
}

func stepMap(value map[string]any) anyMap {
	return anyMap(value)
}

func arrayAny(value any) []any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []any:
		return typed
	default:
		return nil
	}
}

func requireString(m anyMap, key string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("missing object for %s", key)
	}
	return requireStringValue(m[key])
}

func requireStringValue(value any) (string, error) {
	s, ok := value.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("expected non-empty string, got %#v", value)
	}
	return s, nil
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case nil:
		return false
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case string:
		return typed != ""
	default:
		return true
	}
}

func durationMillis(value any) time.Duration {
	switch typed := value.(type) {
	case float64:
		return time.Duration(typed * float64(time.Millisecond))
	case int:
		return time.Duration(typed) * time.Millisecond
	case time.Duration:
		return typed
	default:
		return 0
	}
}

func requireDurationMillis(parent map[string]any, key string) (time.Duration, error) {
	value, ok := parent[key]
	if !ok {
		return 0, fmt.Errorf("missing %s", key)
	}
	switch value.(type) {
	case float64, int, time.Duration:
		return durationMillis(value), nil
	default:
		return 0, fmt.Errorf("%s must be a number of milliseconds", key)
	}
}

func timerValueDuration(value any) (time.Duration, error) {
	switch typed := normalizeJSONValue(value).(type) {
	case int:
		return time.Duration(typed) * time.Millisecond, nil
	case float64:
		return time.Duration(typed * float64(time.Millisecond)), nil
	case time.Duration:
		return typed, nil
	default:
		return 0, fmt.Errorf("invalid interval")
	}
}

func positiveTimerDuration(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 0
	}
	return duration
}

func normalizeJSONValue(value any) any {
	switch typed := value.(type) {
	case dynamicAnyValue:
		return normalizeJSONValue(typed.Value)
	case map[string]any:
		out := anyMap{}
		for k, v := range typed {
			out[k] = normalizeJSONValue(v)
		}
		return out
	case anyMap:
		out := anyMap{}
		for k, v := range typed {
			out[k] = normalizeJSONValue(v)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = normalizeJSONValue(v)
		}
		return out
	case []anyMap:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = normalizeJSONValue(v)
		}
		return out
	case []string:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = v
		}
		return out
	case float64:
		if typed == float64(int64(typed)) {
			return int(typed)
		}
		return typed
	default:
		return value
	}
}

func valueContains(actual, expected any) bool {
	expectedMap, expectedIsMap := expected.(anyMap)
	if expectedIsMap {
		actualMap := object(actual)
		if actualMap == nil {
			return false
		}
		for key, expectedValue := range expectedMap {
			actualValue, ok := actualMap[key]
			if !ok || !valueContains(actualValue, expectedValue) {
				return false
			}
		}
		return true
	}
	expectedList, expectedIsList := expected.([]any)
	if expectedIsList {
		actualList := arrayAny(actual)
		if len(actualList) != len(expectedList) {
			return false
		}
		for index, expectedValue := range expectedList {
			if !valueContains(actualList[index], expectedValue) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(normalizeJSONValue(actual), normalizeJSONValue(expected))
}

func inferAttrType(value any) string {
	switch normalizeJSONValue(value).(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int64, float64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any, anyMap:
		return "object"
	default:
		return ""
	}
}

func valueMatchesAttrType(value any, typ string) bool {
	value = normalizeJSONValue(value)
	switch typ {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean", "bool":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case int, int64, float64, json.Number:
			return true
		default:
			return false
		}
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		switch value.(type) {
		case map[string]any, anyMap:
			return true
		default:
			return false
		}
	default:
		return true
	}
}

func mustJSON(value any) string {
	data, err := json.MarshalIndent(normalizeJSONValue(value), "", "  ")
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	return string(data)
}
