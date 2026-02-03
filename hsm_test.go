package hsm_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stateforward/hsm.go"
)

type Trace struct {
	sync  []string
	async []string
	mutex *sync.Mutex
}

func (t *Trace) reset() {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.sync = []string{}
	t.async = []string{}
}

func (t *Trace) matches(expected Trace) bool {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if expected.sync != nil && !slices.Equal(t.sync, expected.sync) {
		return false
	}
	if expected.async != nil && !slices.Equal(t.async, expected.async) {
		return false
	}
	return true
}

func (t *Trace) contains(expected Trace) bool {
	if expected.sync != nil && slices.ContainsFunc(t.sync, func(s string) bool {
		return slices.Contains(expected.sync, s)
	}) {
		return true
	}
	if expected.async != nil && slices.ContainsFunc(t.async, func(s string) bool {
		return slices.Contains(expected.async, s)
	}) {
		return true
	}
	return false
}

type Event struct{}

type THSM struct {
	hsm.HSM
	foo int
}

type AttrHSM struct {
	hsm.HSM
}

type CallOrderHSM struct {
	hsm.HSM
	mu    sync.Mutex
	order []string
}

func (sm *CallOrderHSM) record(step string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.order = append(sm.order, step)
}

func (sm *CallOrderHSM) orderSnapshot() []string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return append([]string(nil), sm.order...)
}

type CallSigHSM struct {
	hsm.HSM
	mu   sync.Mutex
	hits []string
}

func (sm *CallSigHSM) record(hit string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.hits = append(sm.hits, hit)
}

func (sm *CallSigHSM) hitsSnapshot() []string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return append([]string(nil), sm.hits...)
}

func (sm *CallSigHSM) methodExpr(arg string) string {
	sm.record("methodExpr")
	return "method:" + arg
}

func assertPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for %s", name)
		}
	}()
	fn()
}

func TestComplex(t *testing.T) {
	trace := &Trace{
		mutex: &sync.Mutex{},
	}
	// test
	mockAction := func(name string, async bool) func(ctx context.Context, thsm *THSM, event hsm.Event) {
		return func(ctx context.Context, thsm *THSM, event hsm.Event) {
			trace.mutex.Lock()
			defer trace.mutex.Unlock()
			if async {
				trace.async = append(trace.async, name)
			} else {
				trace.sync = append(trace.sync, name)
			}
		}
	}
	afterTriggered := false
	dEvent := hsm.Event{
		Name: "D",
	}
	iEvent := hsm.Event{
		Name: "I",
	}
	aEvent := hsm.Event{
		Name: "A",
	}
	gEvent := hsm.Event{
		Name: "G",
	}
	cEvent := hsm.Event{
		Name: "C",
	}
	uEvent := hsm.Event{
		Name: "u.t",
	}
	xEvent := hsm.Event{
		Name: "X",
	}
	eEvent := hsm.Event{
		Name: "E",
	}
	hEvent := hsm.Event{
		Name: "H",
	}
	jEvent := hsm.Event{
		Name: "J",
	}
	kEvent := hsm.Event{
		Name: "K",
	}
	zEvent := hsm.Event{
		Name: "Z",
	}
	ctx := context.Background()
	model := hsm.Define(
		"TestHSM",
		hsm.State("s",
			hsm.Entry(mockAction("s.entry", false)),
			hsm.Activity(mockAction("s.activity", true)),
			hsm.Exit(mockAction("s.exit", false)),
			hsm.State("s1",
				hsm.State("s11",
					hsm.Entry(mockAction("s11.entry", false)),
					hsm.Activity(mockAction("s11.activity", true)),
					hsm.Exit(mockAction("s11.exit", false)),
				),
				hsm.Initial(hsm.Target("s11"), hsm.Effect(mockAction("s1.initial.effect", false))),
				hsm.Exit(mockAction("s1.exit", false)),
				hsm.Entry(mockAction("s1.entry", false)),
				hsm.Activity(mockAction("s1.activity", true)),
				hsm.Transition(hsm.On(iEvent), hsm.Effect(mockAction("s1.I.transition.effect", false))),
				hsm.Transition(hsm.On(aEvent), hsm.Target("/s/s1"), hsm.Effect(mockAction("s1.A.transition.effect", false))),
			),
			hsm.Transition(hsm.On(dEvent), hsm.Source("/s/s1/s11"), hsm.Target("/s/s1"), hsm.Effect(mockAction("s11.D.transition.effect", false)), hsm.Guard(
				func(ctx context.Context, hsm *THSM, event hsm.Event) bool {
					check := hsm.foo == 1
					hsm.foo = 0
					return check
				},
			)),
			hsm.Initial(hsm.Target("s1/s11"), hsm.Effect(mockAction("s.initial.effect", false))),
			hsm.State("s2",
				hsm.Entry(mockAction("s2.entry", false)),
				hsm.Activity(mockAction("s2.activity", true)),
				hsm.Exit(mockAction("s2.exit", false)),
				hsm.State("s21",
					hsm.State("s211",
						hsm.Entry(mockAction("s211.entry", false)),
						hsm.Activity(mockAction("s211.activity", true)),
						hsm.Exit(mockAction("s211.exit", false)),
						hsm.Transition(hsm.On(gEvent), hsm.Target("/s/s1/s11"), hsm.Effect(mockAction("s211.G.transition.effect", false))),
					),
					hsm.Initial(hsm.Target("s211"), hsm.Effect(mockAction("s21.initial.effect", false))),
					hsm.Entry(mockAction("s21.entry", false)),
					hsm.Activity(mockAction("s21.activity", true)),
					hsm.Exit(mockAction("s21.exit", false)),
					hsm.Transition(hsm.On(aEvent), hsm.Target("/s/s2/s21")), // self transition
				),
				hsm.Initial(hsm.Target("s21/s211"), hsm.Effect(mockAction("s2.initial.effect", false))),
				hsm.Transition(hsm.On(cEvent), hsm.Target("/s/s1"), hsm.Effect(mockAction("s2.C.transition.effect", false))),
			),
			hsm.State("s3",
				hsm.Entry(mockAction("s3.entry", false)),
				hsm.Activity(mockAction("s3.activity", true)),
				hsm.Exit(mockAction("s3.exit", false)),
			),
			// Wildcard events are no longer supported
			// hsm.Transition(hsm.On(`*.P.*`), hsm.Effect(mockAction("s11.P.transition.effect", false))),
		),
		hsm.State("t",
			hsm.Entry(mockAction("t.entry", false)),
			hsm.Activity(mockAction("t.activity", true)),
			hsm.Exit(mockAction("t.exit", false)),
			hsm.State(
				"u",
				hsm.Entry(mockAction("u.entry", false)),
				hsm.Activity(mockAction("u.activity", true)),
				hsm.Exit(mockAction("u.exit", false)),
				hsm.Transition(
					hsm.On(uEvent),
					hsm.Target("/t"),
					hsm.Effect(mockAction("u.t.transition.effect", false)),
				),
			),
			hsm.Transition(
				hsm.On(xEvent),
				hsm.Target("/exit"),
				hsm.Effect(mockAction("u.X.transition.effect", false)),
			),
		),

		hsm.Final("exit"),
		hsm.Initial(
			hsm.Target(hsm.Choice(
				"initial_choice",
				hsm.Transition(hsm.Target("/s/s2")),
			)), hsm.Effect(mockAction("initial.effect", false))),
		hsm.Transition(hsm.On(dEvent), hsm.Source("/s/s1"), hsm.Target("/s"), hsm.Effect(mockAction("s1.D.transition.effect", false)), hsm.Guard(
			func(ctx context.Context, hsm *THSM, event hsm.Event) bool {
				check := hsm.foo == 0
				hsm.foo++
				return check
			},
		)),
		// Wildcard events are no longer supported
		// hsm.Transition("wildcard", hsm.On("abcd*"), hsm.Source("/s"), hsm.Target("/s")),
		hsm.Transition(hsm.On(dEvent), hsm.Source("/s"), hsm.Target("/s"), hsm.Effect(mockAction("s.D.transition.effect", false))),
		hsm.Transition(hsm.On(cEvent), hsm.Source("/s/s1"), hsm.Target("/s/s2"), hsm.Effect(mockAction("s1.C.transition.effect", false))),
		hsm.Transition(hsm.On(eEvent), hsm.Source("/s"), hsm.Target("/s/s1/s11"), hsm.Effect(mockAction("s.E.transition.effect", false))),
		hsm.Transition(hsm.On(gEvent), hsm.Source("/s/s1/s11"), hsm.Target("/s/s2/s21/s211"), hsm.Effect(mockAction("s11.G.transition.effect", false))),
		hsm.Transition(hsm.On(iEvent), hsm.Source("/s"), hsm.Effect(mockAction("s.I.transition.effect", false)), hsm.Guard(
			func(ctx context.Context, hsm *THSM, event hsm.Event) bool {
				check := hsm.foo == 0
				hsm.foo++
				return check
			},
		)),
		hsm.Transition(hsm.After(
			func(ctx context.Context, hsm *THSM, event hsm.Event) time.Duration {
				return time.Second * 2
			},
		), hsm.Source("/s/s2/s21/s211"), hsm.Target("/s/s1/s11"), hsm.Effect(mockAction("s211.after.transition.effect", false)), hsm.Guard(
			func(ctx context.Context, hsm *THSM, event hsm.Event) bool {
				triggered := !afterTriggered
				afterTriggered = true
				return triggered
			},
		)),
		hsm.Transition(hsm.On(hEvent), hsm.Source("/s/s1/s11"), hsm.Target(
			hsm.Choice(
				hsm.Transition(hsm.Target("/s/s1"), hsm.Guard(
					func(ctx context.Context, hsm *THSM, event hsm.Event) bool {
						return hsm.foo == 0
					},
				)),
				hsm.Transition(hsm.Target("/s/s2"), hsm.Effect(mockAction("s11.H.choice.transition.effect", false))),
			),
		), hsm.Effect(mockAction("s11.H.transition.effect", false))),
		hsm.Transition(hsm.On(jEvent), hsm.Source("/s/s2/s21/s211"), hsm.Target("/s/s1/s11"), hsm.Effect(func(ctx context.Context, thsm *THSM, event hsm.Event) {
			trace.async = append(trace.async, "s11.J.transition.effect")
			hsm.Dispatch(ctx, thsm, hsm.Event{
				Name: "K",
			})
		})),
		hsm.Transition(hsm.On(kEvent), hsm.Source("/s/s1/s11"), hsm.Target("/s/s3"), hsm.Effect(mockAction("s11.K.transition.effect", false))),
		hsm.Transition(hsm.On(zEvent), hsm.Effect(mockAction("Z.transition.effect", false))),
		hsm.Transition(hsm.On(xEvent), hsm.Effect(mockAction("X.transition.effect", false)), hsm.Source("/s/s3"), hsm.Target("/t/u")),
	)
	sm := hsm.New(&THSM{
		foo: 0,
	}, &model, hsm.Config{
		Name: "TestHSM",
		ID:   "test",
	})
	hsm.Start(ctx, sm)
	if sm.State() != "/TestHSM/s/s2/s21/s211" {
		t.Fatal("Initial state is not /TestHSM/s/s2/s21/s211", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"initial.effect", "s.entry", "s2.entry", "s2.initial.effect", "s21.entry", "s211.entry"},
	}) {
		t.Fatal("Trace is not correct", "trace", trace)
	}

	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "G",
	})
	if sm.State() != "/TestHSM/s/s1/s11" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s211.exit", "s21.exit", "s2.exit", "s211.G.transition.effect", "s1.entry", "s11.entry"},
	}) {
		t.Fatal("trace is not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "I",
	})
	if sm.State() != "/TestHSM/s/s1/s11" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s1.I.transition.effect"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "A",
	})
	if sm.State() != "/TestHSM/s/s1/s11" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s11.exit", "s1.exit", "s1.A.transition.effect", "s1.entry", "s1.initial.effect", "s11.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "D",
	})
	if sm.State() != "/TestHSM/s" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s11.exit", "s1.exit", "s1.D.transition.effect"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "D",
	})
	if sm.State() != "/TestHSM/s/s1/s11" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s.exit", "s.D.transition.effect", "s.entry", "s.initial.effect", "s1.entry", "s11.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "D",
	})
	if sm.State() != "/TestHSM/s/s1" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s11.exit", "s11.D.transition.effect"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "C",
	})
	if sm.State() != "/TestHSM/s/s2/s21/s211" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s1.exit", "s1.C.transition.effect", "s2.entry", "s2.initial.effect", "s21.entry", "s211.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "E",
	})
	if !hsm.Match(sm.State(), "/TestHSM/s/s1/s11") {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s211.exit", "s21.exit", "s2.exit", "s.E.transition.effect", "s1.entry", "s11.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "E",
	})
	if sm.State() != "/TestHSM/s/s1/s11" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s11.exit", "s1.exit", "s.E.transition.effect", "s1.entry", "s11.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "G",
	})
	if sm.State() != "/TestHSM/s/s2/s21/s211" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s11.exit", "s1.exit", "s11.G.transition.effect", "s2.entry", "s21.entry", "s211.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "I",
	})
	if sm.State() != "/TestHSM/s/s2/s21/s211" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s.I.transition.effect"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	time.Sleep(time.Second * 3)
	if !trace.matches(Trace{
		sync: []string{"s211.exit", "s21.exit", "s2.exit", "s211.after.transition.effect", "s1.entry", "s11.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "H",
	})
	if sm.State() != "/TestHSM/s/s2/s21/s211" {
		t.Fatal("state is not correct after H", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s11.H.transition.effect", "s11.exit", "s1.exit", "s11.H.choice.transition.effect", "s2.entry", "s2.initial.effect", "s21.entry", "s211.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "J",
	})
	if sm.State() != "/TestHSM/s/s3" {
		t.Fatal("state is not correct after J expected /s/s3 got", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s211.exit", "s21.exit", "s2.exit", "s1.entry", "s11.entry", "s11.exit", "s1.exit", "s11.K.transition.effect", "s3.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	// Wildcard events are no longer supported
	// trace.reset()
	// <-sm.Dispatch(ctx, hsm.Event{
	// 	Name: "K.P.A",
	// })
	// if !trace.contains(Trace{
	// 	sync: []string{"s11.P.transition.effect"},
	// }) {
	// 	t.Fatal("transition actions are not correct", "trace", trace)
	// }
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{Name: "Z"})
	if sm.State() != "/TestHSM/s/s3" {
		t.Fatal("state is not correct after Z", "state", sm.State())
	}
	if !trace.contains(
		Trace{
			sync: []string{"Z.transition.effect"},
		},
	) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "X",
	})
	if sm.State() != "/TestHSM/t/u" {
		t.Fatal("state is not correct after X", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"s3.exit", "s.exit", "X.transition.effect", "t.entry", "u.entry"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "u.t",
	})
	if sm.State() != "/TestHSM/t" {
		t.Fatal("state is not correct after u.t", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"u.exit", "u.t.transition.effect"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	trace.reset()
	<-hsm.Dispatch(ctx, sm, hsm.Event{
		Name: "X",
	})
	if sm.State() != "/TestHSM/exit" {
		t.Fatal("state is not correct after X", "state", sm.State())
	}
	if !trace.matches(Trace{
		sync: []string{"t.exit", "u.X.transition.effect"},
	}) {
		t.Fatal("transition actions are not correct", "trace", trace)
	}
	select {
	case <-sm.Context().Done():
	default:
		t.Fatal("sm is not done after entering top level final state")
	}
	trace.reset()
	<-hsm.Stop(ctx, sm)
	if sm.State() != "/TestHSM" {
		t.Fatal("state is not correct", "state", sm.State())
	}

}

func TestHSMDispatchAll(t *testing.T) {
	fooEvent := hsm.Event{
		Name: "foo",
	}
	barEvent := hsm.Event{
		Name: "bar",
	}
	model := hsm.Define(
		"TestHSM",
		hsm.State("foo"),
		hsm.State("bar"),
		hsm.Transition(hsm.On(fooEvent), hsm.Source("foo"), hsm.Target("bar")),
		hsm.Transition(hsm.On(barEvent), hsm.Source("bar"), hsm.Target("foo")),
		hsm.Initial(hsm.Target("foo")),
	)
	ctx := context.Background()
	sm1 := hsm.Started(ctx, &THSM{}, &model)
	sm2 := hsm.Started(sm1.Context(), &THSM{}, &model)
	if sm2.State() != "/TestHSM/foo" {
		t.Fatal("state is not correct", "state", sm2.State())
	}
	hsm.DispatchAll(sm2.Context(), hsm.Event{
		Name: "foo",
	})
	time.Sleep(time.Second)
	if sm1.State() != "/TestHSM/bar" {
		t.Fatal("state is not correct", "state", sm1.State())
	}
	if sm2.State() != "/TestHSM/bar" {
		t.Fatal("state is not correct", "state", sm2.State())
	}
}

func TestEvery(t *testing.T) {
	timestamps := []time.Time{}
	mutex := sync.Mutex{}
	model := hsm.Define(
		"TestHSM",
		hsm.Initial(hsm.Target("foo")),
		hsm.State("foo"),
		hsm.Transition(
			hsm.Every(func(ctx context.Context, thsm *THSM, event hsm.Event) time.Duration {
				return time.Millisecond * 500
			}),
			hsm.Effect(func(ctx context.Context, thsm *THSM, event hsm.Event) {
				mutex.Lock()
				defer mutex.Unlock()
				timestamps = append(timestamps, time.Now())
			}),
		),
	)
	_ = hsm.Started(context.Background(), &THSM{}, &model)
	for i := 0; i < 10; i++ {
		time.Sleep(time.Millisecond * 550)
		mutex.Lock()
		if len(timestamps) > i+1 {
			t.Fatalf("timestamps are not in order expected %d got %d", i+1, len(timestamps))
		}
		mutex.Unlock()
	}
	mutex.Lock()
	defer mutex.Unlock()
	for i := 1; i < len(timestamps)-1; i++ {
		delta := timestamps[i+1].Sub(timestamps[i])
		if delta < time.Millisecond*500 || delta > time.Millisecond*551 {
			t.Fatalf("delta is not correct expected %v got %v", time.Millisecond*500, delta)
		}
	}
}

func TestNeverAfter(t *testing.T) {
	effect := atomic.Bool{}
	model := hsm.Define(
		"TestHSM",
		hsm.Initial(hsm.Target("foo")),
		hsm.State("foo",
			hsm.Transition(
				hsm.After(func(ctx context.Context, sm *THSM, event hsm.Event) time.Duration {
					return time.Second * -1
				}),
				hsm.Effect(func(ctx context.Context, sm *THSM, event hsm.Event) {
					effect.Store(true)
				}),
			),
		),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	if sm.State() != "/TestHSM/foo" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	time.Sleep(time.Second * 2)
	if effect.Load() {
		t.Fatal("effect should not be called")
	}
}

func TestDispatchTo(t *testing.T) {
	fooEvent := hsm.Event{
		Name: "foo",
	}
	barEvent := hsm.Event{
		Name: "bar",
	}
	model := hsm.Define(
		"TestHSM",
		hsm.State("foo"),
		hsm.State("bar"),
		hsm.Transition(hsm.On(fooEvent), hsm.Source("foo"), hsm.Target("bar")),
		hsm.Transition(hsm.On(barEvent), hsm.Source("bar"), hsm.Target("foo")),
		hsm.Initial(hsm.Target("foo")),
	)
	ctx := context.Background()
	sm1 := hsm.Started(ctx, &THSM{}, &model, hsm.Config{ID: "sm1"})
	sm2 := hsm.Started(sm1.Context(), &THSM{}, &model, hsm.Config{ID: "sm2"})
	if sm1.State() != "/TestHSM/foo" {
		t.Fatal("state is not correct", "state", sm1.State())
	}
	if sm2.State() != "/TestHSM/foo" {
		t.Fatal("state is not correct", "state", sm2.State())
	}
	<-hsm.DispatchTo(sm2.Context(), hsm.Event{
		Name: "foo",
	}, "sm*")
	if sm2.State() != "/TestHSM/bar" {
		t.Fatal("state is not correct", "state", sm2.State())
	}
	if sm1.State() != "/TestHSM/bar" {
		t.Fatal("state is not correct", "state", sm1.State())
	}
}

func noBehavior(ctx context.Context, hsm *THSM, event hsm.Event) {
	// slog.Info("noBehavior", "event", event.Name)
}

func TestChoiceBackToSource(t *testing.T) {
	actions := atomic.Value{}
	actions.Store([]string{})

	makeBehavior := func(name string) func(ctx context.Context, hsm *THSM, event hsm.Event) {
		return func(ctx context.Context, hsm *THSM, event hsm.Event) {
			slog.Info("behavior", "name", name)
			actions.Store(append(actions.Load().([]string), name))
		}
	}
	choiceEvent := hsm.Event{
		Name: "choice",
	}
	model := hsm.Define(
		"TestHSM",
		hsm.Initial(hsm.Target("foo")),
		hsm.State("foo", hsm.Entry(makeBehavior("foo.entry")), hsm.Exit(makeBehavior("foo.exit")), hsm.Choice(
			"choice",
			hsm.Transition(
				hsm.Target("../bar"),
				hsm.Guard(func(ctx context.Context, hsm *THSM, event hsm.Event) bool {
					return false
				}),
			),
			hsm.Transition(hsm.Target("../foo"), hsm.Effect(makeBehavior("foo.choice.effect"))),
		)),
		hsm.State("bar", hsm.Entry(makeBehavior("bar.entry")), hsm.Exit(makeBehavior("bar.exit"))),
		hsm.Transition(hsm.On(choiceEvent), hsm.Source("foo"), hsm.Target("foo/choice")),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	if sm.State() != "/TestHSM/foo" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	actions.Store([]string{})
	<-hsm.Dispatch(context.Background(), sm, hsm.Event{
		Name: "choice",
	})
	if sm.State() != "/TestHSM/foo" {
		t.Fatal("state is not correct", "state", sm.State())
	}
	slog.Info("actions", "actions", actions.Load())

}

func TestIsAncestor(t *testing.T) {
	if !hsm.IsAncestor("/foo/bar", "/foo/bar/baz") {
		t.Fatal("IsAncestor is not correct /foo/bar is an ancestor of /foo/bar/baz")
	}
	if hsm.IsAncestor("/foo/bar/baz", "/foo/bar") {
		t.Fatal("IsAncestor is not correct /foo/bar/baz is not an ancestor of /foo/bar")
	}
	if hsm.IsAncestor("/foo/bar/baz", "/foo/bar/baz") {
		t.Fatal("IsAncestor is not correct /foo/bar/baz is not an ancestor of /foo/bar/baz")
	}
	if !hsm.IsAncestor("/foo/bar/baz", "/foo/bar/baz/qux") {
		t.Fatal("IsAncestor is not correct /foo/bar/baz is an ancestor of /foo/bar/baz/qux")
	}
	if !hsm.IsAncestor("/", "/foo/bar/baz/qux") {
		t.Fatal("IsAncestor is not correct / is an ancestor of /foo/bar/baz/qux")
	}
	if !hsm.IsAncestor("/foo/", "/foo/bar/baz/qux") {
		t.Fatal("IsAncestor is not correct /foo/ is an ancestor of /foo/bar/baz/qux")
	}
}

func TestInitialEventData(t *testing.T) {
	type data struct {
		foo string
	}
	var configData atomic.Pointer[data]
	fooEvent := hsm.Event{
		Name: "foo",
	}
	barEvent := hsm.Event{
		Name: "bar",
	}
	model := hsm.Define(
		"TestHSM",
		hsm.Initial(hsm.Target("foo"), hsm.Effect(func(ctx context.Context, sm *THSM, event hsm.Event) {
			configData.Store(event.Data.(*data))
		})),
		hsm.State("foo"),
		hsm.State("bar"),
		hsm.Transition(hsm.On(fooEvent), hsm.Source("foo"), hsm.Target("bar")),
		hsm.Transition(hsm.On(barEvent), hsm.Source("bar"), hsm.Target("foo")),
	)
	ctx := context.Background()
	hsm.Started(ctx, &THSM{}, &model, hsm.Config{
		Data: &data{
			foo: "testing",
		},
	})
	if configData.Load().foo != "testing" {
		t.Fatal("config data is not correct", "config data", configData.Load())
	}
}

func TestAttributeDefaultAndOnSet(t *testing.T) {
	changeCh := make(chan hsm.AttributeChange, 1)
	kindCh := make(chan uint64, 1)
	model := hsm.Define(
		"AttrHSM",
		hsm.Attribute("count", 1),
		hsm.State("idle",
			hsm.Transition(
				hsm.OnSet("count"),
				hsm.Target("../changed"),
				hsm.Effect(func(ctx context.Context, sm *AttrHSM, event hsm.Event) {
					if change, ok := event.Data.(hsm.AttributeChange); ok {
						changeCh <- change
					}
					kindCh <- event.Kind
				}),
			),
		),
		hsm.State("changed"),
		hsm.Initial(hsm.Target("idle")),
	)
	sm := hsm.Started(context.Background(), &AttrHSM{}, &model)
	if value, ok := sm.Get("count"); !ok || value.(int) != 1 {
		t.Fatal("expected default attribute value", "value", value, "ok", ok)
	}
	<-sm.Set(context.Background(), "count", 1)
	if sm.State() != "/AttrHSM/idle" {
		t.Fatal("state changed on identical attribute set", "state", sm.State())
	}
	select {
	case <-changeCh:
		t.Fatal("unexpected attribute change event for identical value")
	default:
	}
	<-sm.Set(context.Background(), "count", 2)
	if sm.State() != "/AttrHSM/changed" {
		t.Fatal("state did not transition on attribute change", "state", sm.State())
	}
	var change hsm.AttributeChange
	select {
	case change = <-changeCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for attribute change event")
	}
	if change.Name != "count" {
		t.Fatal("attribute change name mismatch", "name", change.Name)
	}
	oldValue, ok := change.Old.(int)
	if !ok || oldValue != 1 {
		t.Fatal("attribute change old value mismatch", "old", change.Old)
	}
	newValue, ok := change.New.(int)
	if !ok || newValue != 2 {
		t.Fatal("attribute change new value mismatch", "new", change.New)
	}
	var kind uint64
	select {
	case kind = <-kindCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for attribute change kind")
	}
	if kind != hsm.ChangeEventKind {
		t.Fatal("attribute change kind mismatch", "kind", kind)
	}
}

func TestOnSetImplicitAttribute(t *testing.T) {
	changeCh := make(chan hsm.AttributeChange, 1)
	model := hsm.Define(
		"AttrImplicitHSM",
		hsm.State("idle",
			hsm.Transition(
				hsm.OnSet("dynamic"),
				hsm.Target("../changed"),
				hsm.Effect(func(ctx context.Context, sm *AttrHSM, event hsm.Event) {
					if change, ok := event.Data.(hsm.AttributeChange); ok {
						changeCh <- change
					}
				}),
			),
		),
		hsm.State("changed"),
		hsm.Initial(hsm.Target("idle")),
	)
	sm := hsm.Started(context.Background(), &AttrHSM{}, &model)
	if _, ok := sm.Get("dynamic"); ok {
		t.Fatal("expected no default for implicit attribute")
	}
	<-sm.Set(context.Background(), "dynamic", 42)
	if sm.State() != "/AttrImplicitHSM/changed" {
		t.Fatal("state did not transition on implicit attribute change", "state", sm.State())
	}
	var change hsm.AttributeChange
	select {
	case change = <-changeCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for implicit attribute change")
	}
	if change.Name != "dynamic" {
		t.Fatal("implicit attribute change name mismatch", "name", change.Name)
	}
	if change.Old != nil {
		t.Fatal("implicit attribute old value should be nil", "old", change.Old)
	}
	newValue, ok := change.New.(int)
	if !ok || newValue != 42 {
		t.Fatal("implicit attribute new value mismatch", "new", change.New)
	}
}

func TestAttributeValidation(t *testing.T) {
	assertPanic(t, "empty attribute name", func() {
		hsm.Define(
			"BadAttrEmpty",
			hsm.Attribute(""),
			hsm.State("s"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "attribute name with slash", func() {
		hsm.Define(
			"BadAttrSlash",
			hsm.Attribute("bad/name"),
			hsm.State("s"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "duplicate attribute", func() {
		hsm.Define(
			"BadAttrDup",
			hsm.Attribute("dup"),
			hsm.Attribute("dup"),
			hsm.State("s"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "OnSet outside Transition", func() {
		hsm.Define(
			"BadOnSetOwner",
			hsm.State("s", hsm.OnSet("attr")),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "OnSet empty name", func() {
		hsm.Define(
			"BadOnSetEmpty",
			hsm.State("s", hsm.Transition(hsm.OnSet(""), hsm.Target("../t"))),
			hsm.State("t"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "OnSet name with slash", func() {
		hsm.Define(
			"BadOnSetSlash",
			hsm.State("s", hsm.Transition(hsm.OnSet("bad/name"), hsm.Target("../t"))),
			hsm.State("t"),
			hsm.Initial(hsm.Target("s")),
		)
	})
}

func TestCallOperationAndOnCallTransition(t *testing.T) {
	callDataCh := make(chan hsm.CallData, 1)
	kindCh := make(chan uint64, 1)
	sourceCh := make(chan string, 1)
	model := hsm.Define(
		"CallHSM",
		hsm.OperationDef("do", func(ctx context.Context, sm *CallOrderHSM, a int, b string) string {
			sm.record("op")
			return fmt.Sprintf("%d:%s", a, b)
		}),
		hsm.State("idle",
			hsm.Transition(
				hsm.OnCall("do"),
				hsm.Target("../called"),
				hsm.Effect(func(ctx context.Context, sm *CallOrderHSM, event hsm.Event) {
					sm.record("effect")
					if data, ok := event.Data.(hsm.CallData); ok {
						callDataCh <- data
					}
					kindCh <- event.Kind
					sourceCh <- event.Source
				}),
			),
		),
		hsm.State("called"),
		hsm.Initial(hsm.Target("idle")),
	)
	sm := hsm.Started(context.Background(), &CallOrderHSM{}, &model)
	result, err := sm.Call(context.Background(), "do", 1, "two")
	if err != nil {
		t.Fatal("call returned error", "err", err)
	}
	if result != "1:two" {
		t.Fatal("call result mismatch", "result", result)
	}
	deadline := time.After(time.Second)
	for sm.State() != "/CallHSM/called" {
		select {
		case <-deadline:
			t.Fatal("state did not transition on OnCall", "state", sm.State())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	var data hsm.CallData
	select {
	case data = <-callDataCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for call data")
	}
	if data.Name != "do" {
		t.Fatal("call data name mismatch", "name", data.Name)
	}
	if len(data.Args) != 2 {
		t.Fatal("call data args length mismatch", "len", len(data.Args))
	}
	if data.Args[0].(int) != 1 || data.Args[1].(string) != "two" {
		t.Fatal("call data args mismatch", "args", data.Args)
	}
	var kind uint64
	select {
	case kind = <-kindCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for call kind")
	}
	if kind != hsm.CallEventKind {
		t.Fatal("call event kind mismatch", "kind", kind)
	}
	var source string
	select {
	case source = <-sourceCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for call source")
	}
	if source != "do" {
		t.Fatal("call event source mismatch", "source", source)
	}
	order := sm.orderSnapshot()
	if len(order) != 2 || !slices.Contains(order, "effect") || !slices.Contains(order, "op") {
		t.Fatal("call order mismatch", "order", order)
	}
}

func TestCallSignatureMatchingAndErrors(t *testing.T) {
	type ctxKey string
	opErr := errors.New("op error")
	model := hsm.Define(
		"CallSigHSM",
		hsm.OperationDef("argsOnly", func(a int, b string) string {
			return fmt.Sprintf("%d:%s", a, b)
		}),
		hsm.OperationDef("ctxOnly", func(ctx context.Context) string {
			return ctx.Value(ctxKey("k")).(string)
		}),
		hsm.OperationDef("instanceOnly", func(sm *CallSigHSM) string {
			sm.record("instanceOnly")
			return "instance"
		}),
		hsm.OperationDef("methodExpr", (*CallSigHSM).methodExpr),
		hsm.OperationDef("variadic", func(prefix string, nums ...int) int {
			if prefix != "sum" {
				return -1
			}
			total := 0
			for _, n := range nums {
				total += n
			}
			return total
		}),
		hsm.OperationDef("errorOnly", func() error {
			return opErr
		}),
		hsm.State("idle"),
		hsm.Initial(hsm.Target("idle")),
	)
	ctx := context.WithValue(context.Background(), ctxKey("k"), "ctx-ok")
	sm := hsm.Started(ctx, &CallSigHSM{}, &model)
	result, err := sm.Call(ctx, "argsOnly", 3, "ok")
	if err != nil {
		t.Fatal("argsOnly error", "err", err)
	}
	if result != "3:ok" {
		t.Fatal("argsOnly result mismatch", "result", result)
	}
	result, err = sm.Call(ctx, "ctxOnly")
	if err != nil {
		t.Fatal("ctxOnly error", "err", err)
	}
	if result != "ctx-ok" {
		t.Fatal("ctxOnly result mismatch", "result", result)
	}
	result, err = sm.Call(ctx, "instanceOnly")
	if err != nil {
		t.Fatal("instanceOnly error", "err", err)
	}
	if result != "instance" {
		t.Fatal("instanceOnly result mismatch", "result", result)
	}
	result, err = sm.Call(ctx, "methodExpr", "x")
	if err != nil {
		t.Fatal("methodExpr error", "err", err)
	}
	if result != "method:x" {
		t.Fatal("methodExpr result mismatch", "result", result)
	}
	result, err = sm.Call(ctx, "variadic", "sum", 1, 2, 3)
	if err != nil {
		t.Fatal("variadic error", "err", err)
	}
	if result != 6 {
		t.Fatal("variadic result mismatch", "result", result)
	}
	_, err = sm.Call(ctx, "errorOnly")
	if !errors.Is(err, opErr) {
		t.Fatal("errorOnly error mismatch", "err", err)
	}
	_, err = sm.Call(ctx, "argsOnly", "bad", "ok")
	if !errors.Is(err, hsm.ErrInvalidOperation) {
		t.Fatal("expected invalid operation error for bad args", "err", err)
	}
	_, err = sm.Call(ctx, "missing")
	if !errors.Is(err, hsm.ErrMissingOperation) {
		t.Fatal("expected missing operation error", "err", err)
	}
	_, err = sm.Call(ctx, "")
	if !errors.Is(err, hsm.ErrInvalidOperation) {
		t.Fatal("expected invalid operation error for empty name", "err", err)
	}
	if !slices.Contains(sm.hitsSnapshot(), "instanceOnly") {
		t.Fatal("instanceOnly did not record hit")
	}
	if !slices.Contains(sm.hitsSnapshot(), "methodExpr") {
		t.Fatal("methodExpr did not record hit")
	}
}

func TestCallValidation(t *testing.T) {
	assertPanic(t, "operation name empty", func() {
		hsm.Define(
			"BadOpEmpty",
			hsm.OperationDef("", func() {}),
			hsm.State("s"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "operation name with slash", func() {
		hsm.Define(
			"BadOpSlash",
			hsm.OperationDef("bad/name", func() {}),
			hsm.State("s"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "operation duplicate", func() {
		hsm.Define(
			"BadOpDup",
			hsm.OperationDef("dup", func() {}),
			hsm.OperationDef("dup", func() {}),
			hsm.State("s"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "operation not function", func() {
		hsm.Define(
			"BadOpType",
			hsm.OperationDef("bad", 123),
			hsm.State("s"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "OnCall outside Transition", func() {
		hsm.Define(
			"BadOnCallOwner",
			hsm.State("s", hsm.OnCall("do")),
			hsm.State("t"),
			hsm.OperationDef("do", func() {}),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "OnCall empty name", func() {
		hsm.Define(
			"BadOnCallEmpty",
			hsm.OperationDef("do", func() {}),
			hsm.State("s", hsm.Transition(hsm.OnCall(""), hsm.Target("../t"))),
			hsm.State("t"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "OnCall name with slash", func() {
		hsm.Define(
			"BadOnCallSlash",
			hsm.OperationDef("do", func() {}),
			hsm.State("s", hsm.Transition(hsm.OnCall("bad/name"), hsm.Target("../t"))),
			hsm.State("t"),
			hsm.Initial(hsm.Target("s")),
		)
	})
	assertPanic(t, "OnCall missing operation", func() {
		hsm.Define(
			"BadOnCallMissing",
			hsm.State("s", hsm.Transition(hsm.OnCall("missing"), hsm.Target("../t"))),
			hsm.State("t"),
			hsm.Initial(hsm.Target("s")),
		)
	})
}

func TestHistoryRestoresShallowAndDeep(t *testing.T) {
	toA1b := hsm.Event{Name: "toA1b"}
	toB := hsm.Event{Name: "toB"}
	backShallow := hsm.Event{Name: "backShallow"}
	backDeep := hsm.Event{Name: "backDeep"}
	model := hsm.Define(
		"HistoryHSM",
		hsm.State("A",
			hsm.State("A1",
				hsm.State("A1a"),
				hsm.State("A1b"),
				hsm.Initial(hsm.Target("A1a")),
			),
			hsm.State("A2"),
			hsm.ShallowHistory("shallow"),
			hsm.DeepHistory("deep"),
			hsm.Initial(hsm.Target("A1")),
		),
		hsm.State("B"),
		hsm.Transition(hsm.On(toA1b), hsm.Source("A/A1/A1a"), hsm.Target("A/A1/A1b")),
		hsm.Transition(hsm.On(toB), hsm.Source("A/A1/A1b"), hsm.Target("B")),
		hsm.Transition(hsm.On(backDeep), hsm.Source("B"), hsm.Target("A/deep")),
		hsm.Transition(hsm.On(backShallow), hsm.Source("B"), hsm.Target("A/shallow")),
		hsm.Initial(hsm.Target("A")),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	if sm.State() != "/HistoryHSM/A/A1/A1a" {
		t.Fatal("initial state mismatch", "state", sm.State())
	}
	<-hsm.Dispatch(context.Background(), sm, toA1b)
	if sm.State() != "/HistoryHSM/A/A1/A1b" {
		t.Fatal("state mismatch after toA1b", "state", sm.State())
	}
	<-hsm.Dispatch(context.Background(), sm, toB)
	if sm.State() != "/HistoryHSM/B" {
		t.Fatal("state mismatch after toB", "state", sm.State())
	}
	<-hsm.Dispatch(context.Background(), sm, backDeep)
	if sm.State() != "/HistoryHSM/A/A1/A1b" {
		t.Fatal("deep history did not restore leaf state", "state", sm.State())
	}
	<-hsm.Dispatch(context.Background(), sm, toB)
	<-hsm.Dispatch(context.Background(), sm, backShallow)
	if sm.State() != "/HistoryHSM/A/A1/A1a" {
		t.Fatal("shallow history did not restore parent initial", "state", sm.State())
	}
}

func TestHistoryFallbackDefaultAndInitial(t *testing.T) {
	toShallow := hsm.Event{Name: "toShallow"}
	toDeep := hsm.Event{Name: "toDeep"}
	model := hsm.Define(
		"HistoryFallbackHSM",
		hsm.State("A",
			hsm.State("A1"),
			hsm.State("A2"),
			hsm.Initial(hsm.Target("A1")),
			hsm.ShallowHistory("shallow", hsm.Transition(hsm.Target("A2"))),
			hsm.DeepHistory("deep"),
		),
		hsm.State("B"),
		hsm.Transition(hsm.On(toShallow), hsm.Source("B"), hsm.Target("A/shallow")),
		hsm.Transition(hsm.On(toDeep), hsm.Source("B"), hsm.Target("A/deep")),
		hsm.Initial(hsm.Target("B")),
	)

	t.Run("shallowUsesDefaultTransition", func(t *testing.T) {
		sm := hsm.Started(context.Background(), &THSM{}, &model)
		if sm.State() != "/HistoryFallbackHSM/B" {
			t.Fatal("initial state mismatch", "state", sm.State())
		}
		<-hsm.Dispatch(context.Background(), sm, toShallow)
		if sm.State() != "/HistoryFallbackHSM/A/A2" {
			t.Fatal("shallow history did not use default transition", "state", sm.State())
		}
	})

	t.Run("deepUsesParentInitial", func(t *testing.T) {
		sm := hsm.Started(context.Background(), &THSM{}, &model)
		if sm.State() != "/HistoryFallbackHSM/B" {
			t.Fatal("initial state mismatch", "state", sm.State())
		}
		<-hsm.Dispatch(context.Background(), sm, toDeep)
		if sm.State() != "/HistoryFallbackHSM/A/A1" {
			t.Fatal("deep history did not use parent initial", "state", sm.State())
		}
	})
}

func TestLCA(t *testing.T) {
	if hsm.LCA("/foo/bar", "/foo/bar/baz") != "/foo/bar" {
		t.Fatal("LCA is not correct", "LCA", hsm.LCA("/foo/bar", "/foo/bar/baz"))
	}
	if hsm.LCA("/foo/bar/baz", "/foo/bar") != "/foo/bar" {
		t.Fatal("LCA is not correct", "LCA", hsm.LCA("/foo/bar/baz", "/foo/bar"))
	}
	if hsm.LCA("/foo/bar/baz", "/foo/bar/baz") != "/foo/bar" {
		t.Fatal("LCA is not correct", "LCA", hsm.LCA("/foo/bar/baz", "/foo/bar/baz"))
	}
	if hsm.LCA("/foo/bar/baz", "/foo/bar/baz/qux") != "/foo/bar/baz" {
		t.Fatal("LCA is not correct", "LCA", hsm.LCA("/foo/bar/baz", "/foo/bar/baz/qux"))
	}
	if hsm.LCA("/", "/foo/bar/baz/qux") != "/" {
		t.Fatal("LCA is not correct", "LCA", hsm.LCA("/", "/foo/bar/baz/qux"))
	}
	if hsm.LCA("", "/foo/bar/baz/qux") != "/foo/bar/baz/qux" {
		t.Fatal("LCA is not correct", "LCA", hsm.LCA("", "/foo/bar/baz/qux"))
	}
	if hsm.LCA("/foo/bar/baz/qux", "") != "/foo/bar/baz/qux" {
		t.Fatal("LCA is not correct", "LCA", hsm.LCA("/foo/bar/baz/qux", ""))
	}
}

var fooEvent = hsm.Event{
	Name: "foo",
}
var barEvent = hsm.Event{
	Name: "bar",
}

// }
var benchModel = hsm.Define(
	"TestHSM",
	hsm.State("foo",
		hsm.Entry(noBehavior, noBehavior, noBehavior),
		hsm.Exit(noBehavior, noBehavior, noBehavior),
		hsm.Activity(noBehavior, noBehavior, noBehavior),
	),
	hsm.State("bar",
		hsm.Entry(noBehavior),
		hsm.Exit(noBehavior),
		hsm.Activity(noBehavior, noBehavior, noBehavior),
	),
	hsm.Transition(
		hsm.On(fooEvent),
		hsm.Source("foo"),
		hsm.Target("bar"),
		hsm.Effect(noBehavior, noBehavior, noBehavior),
	),
	hsm.Transition(
		hsm.On(barEvent),
		hsm.Source("bar"),
		hsm.Target("foo"),
		hsm.Effect(noBehavior, noBehavior, noBehavior),
	),
	hsm.Initial(hsm.Target("foo"), hsm.Effect(noBehavior)),
)

func TestCompletionEvent(t *testing.T) {
	bEvent := hsm.Event{
		Name: "b",
	}
	cEvent := hsm.Event{
		Name: "c",
	}
	dEvent := hsm.Event{
		Name: "d",
	}
	model := hsm.Define(
		"T",
		hsm.Initial(hsm.Target("a")),
		hsm.State("a", hsm.Transition(hsm.On(bEvent), hsm.Target("../b"))),
		hsm.State("b",
			hsm.Entry(func(ctx context.Context, sm *THSM, event hsm.Event) {
				hsm.Dispatch(ctx, sm, hsm.Event{
					Name: "e",
				})
				hsm.Dispatch(ctx, sm, hsm.Event{
					Name: "c",
					Kind: hsm.CompletionEventKind,
				})
			}),
			hsm.Transition(
				hsm.On(cEvent),
				hsm.Source("."),
				hsm.Target(
					hsm.Choice(
						hsm.Transition(
							hsm.Target("../c"),
							hsm.Guard(func(ctx context.Context, sm *THSM, event hsm.Event) bool {
								return true
							}),
						),
						hsm.Transition(
							hsm.On(dEvent),
							hsm.Target("../d"),
						),
					),
				),
			),
		),
		hsm.State("c", hsm.Entry(
			func(ctx context.Context, sm *THSM, event hsm.Event) {
				hsm.Dispatch(ctx, sm, hsm.Event{
					Name: "e",
				})
				hsm.Dispatch(ctx, sm, hsm.Event{
					Name: "d",
					Kind: hsm.CompletionEventKind,
				})
			},
		),
			hsm.Transition(
				hsm.On(dEvent),
				hsm.Target("../d"),
			),
		),
		hsm.State("d"),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	if sm.State() != "/T/a" {
		t.Fatalf("expected state \"/a\" got \"%s\"", sm.State())
	}
	done := hsm.Dispatch(context.Background(), sm, hsm.Event{
		Name: "b",
	})
	<-done
	if sm.State() != "/T/d" {
		t.Fatalf("expected state \"/d\" got \"%s\"", sm.State())
	}

}

func TestMatch(t *testing.T) {
	const sample = "a/ab/abc/abcd/abcde/abcdef/abcdefg/abcdefgh/abcdefghi/abcdefghij/abcdefghijk/abcdefghijkl/abcdefghijklm/abcdefghijklmn/abcdefghijklmno/abcdefghijklmnop/abcdefghijklmnopq/abcdefghijklmnopqr/abcdefghijklmnopqrs/abcdefghijklmnopqrst/abcdefghijklmnopqrstu/abcdefghijklmnopqrstuv/abcdefghijklmnopqrstuvw/abcdefghijklmnopqrstuvwx/abcdefghijklmnopqrstuvwxy/abcdefghijklmnopqrstuvwxyz"
	if !hsm.Match(sample, "a/*/a*/abcde/*") {
		t.Fatal("expected match for a/*/a*/abcde/*")
	}
	if hsm.Match(sample, "a/*/a*/abcde/") {
		t.Fatal("expected no match for a/*/a*/abcde/")
	}
	if hsm.Match(sample, "a/*/a*/abcde/") {
		t.Fatal("expected no match for a/*/a*/abcde/")
	}
	if !hsm.Match("abc", "a*c") {
		t.Fatal("expected match for abc, a*c")
	}
	if !hsm.Match("abc", "*b*") {
		t.Fatal("expected match for abc, *b*")
	}
	if !hsm.Match("abc", "*c") {
		t.Fatal("expected match for abc, *c")
	}
	if !hsm.Match("abc", "a*") {
		t.Fatal("expected match for abc, a*")
	}
	if !hsm.Match("abc", "*") {
		t.Fatal("expected match for abc, *")
	}
	if !hsm.Match("", "*") {
		t.Fatal("expected match for '', *")
	}
	if !hsm.Match("a", "a") {
		t.Fatal("expected match for a, a")
	}
	if hsm.Match("a", "b") {
		t.Fatal("expected no match for a, b")
	}
	if hsm.Match("a", "") {
		t.Fatal("expected no match for a, ''")
	}
	if hsm.Match("", "a") {
		t.Fatal("expected no match for '', a")
	}
	if !hsm.Match("", "") {
		t.Fatal("expected match for '', ''")
	}
	if hsm.Match("abc", "a*d") {
		t.Fatal("expected no match for abc, a*d")
	}
	if !hsm.Match("abcdef", "ab*ef") {
		t.Fatal("expected match for abcdef, ab*ef")
	}
	if !hsm.Match("abcdef", "a*f") {
		t.Fatal("expected match for abcdef, a*f")
	}
	if !hsm.Match("/initialized/starting", "/initialized/starting") {
		t.Fatal("expected match for /initialized/starting, /initialized/starting")
	}
	if !hsm.Match("abcdef", "*f") {
		t.Fatal("expected match for abcdef, *f")
	}
	if !hsm.Match("abcdef", "a*") {
		t.Fatal("expected match for abcdef, a*")
	}
	if hsm.Match("abcdef", "a*g") {
		t.Fatal("expected no match for abcdef, a*g")
	}
	if !hsm.Match("zzabcdef", "*f") {
		t.Fatal("expected match for zzabcdef, *f")
	}
	if hsm.Match("zzabcdef", "b*") {
		t.Fatal("expected no match for zzabcdef, b*")
	}
	if !hsm.Match(sample, "*xyz") {
		t.Fatal("expected match for sample, *xyz")
	}
	if !hsm.Match(sample, "a*z") {
		t.Fatal("expected match for sample, a*z")
	}
	if !hsm.Match(sample, "a*x*z") {
		t.Fatal("expected match for sample, a*x*z")
	}
	if !hsm.Match(sample, "a*x*y*z") {
		t.Fatal("expected match for sample, a*x*y*z")
	}

}

func BenchmarkTransition(b *testing.B) {
	ctx := context.Background()
	var counter atomic.Int64
	behavior := func(ctx context.Context, hsm *THSM, event hsm.Event) {
		counter.Add(1)
	}
	var benchModel = hsm.Define(
		"BenchmarkExecutionHSM",
		hsm.State("foo",
			hsm.Entry(behavior),
			hsm.Exit(behavior),
			hsm.Activity(behavior),
		),
		hsm.State("bar",
			hsm.Entry(behavior),
			hsm.Exit(behavior),
			hsm.Activity(behavior),
		),
		hsm.Transition(
			hsm.On(fooEvent),
			hsm.Source("foo"),
			hsm.Target("bar"),
			hsm.Effect(behavior),
		),
		hsm.Transition(
			hsm.On(barEvent),
			hsm.Source("bar"),
			hsm.Target("foo"),
			hsm.Effect(behavior),
		),
		hsm.Initial(hsm.Target("foo")),
	)
	fooEvent := hsm.Event{
		Name: "foo",
	}
	barEvent := hsm.Event{
		Name: "bar",
	}
	benchSM := hsm.Started(ctx, &THSM{}, &benchModel, hsm.Config{
		ActivityTimeout: 1 * time.Second,
	})
	b.ReportAllocs()
	b.ResetTimer()
	counter.Store(0)
	for i := 0; i < b.N; i++ {
		hsm.Dispatch(ctx, benchSM, fooEvent)
		hsm.Dispatch(ctx, benchSM, barEvent)
	}
	// slog.Info("stopping", "snapshot", hsm.TakeSnapshot(ctx, benchSM), "counter", counter.Load())
	<-hsm.Stop(ctx, benchSM)
	slog.Info("stopped", "snapshot", hsm.TakeSnapshot(ctx, benchSM), "counter", counter.Load(), "count", b.N*2*4)
}

func nonHSMLogic() func(event *hsm.Event) bool {
	type state int
	const (
		foo state = iota
		bar
	)
	currentState := foo
	// Simulating entry/exit actions as no-ops to match HSM version
	fooEntry := func(event *hsm.Event) {}
	fooExit := func(event *hsm.Event) {}
	barEntry := func(event *hsm.Event) {}
	barExit := func(event *hsm.Event) {}
	initialEffect := func(event *hsm.Event) {}

	// Transition effects as no-ops
	fooToBarEffect := func(event *hsm.Event) {}
	barToFooEffect := func(event *hsm.Event) {}

	// Event handling
	handleEvent := func(event *hsm.Event) bool {
		switch currentState {
		case foo:
			if event.Name == "foo" {
				fooExit(event)
				fooToBarEffect(event)
				currentState = bar
				barEntry(event)
				return true
			}
		case bar:
			if event.Name == "bar" {
				barExit(event)
				barToFooEffect(event)
				currentState = foo
				fooEntry(event)
				return true
			}
		}
		return false
	}

	// Initial transition
	initialEffect(nil)
	fooEntry(nil)
	return handleEvent
}

func TestWhen(t *testing.T) {
	model := hsm.Define(
		"TestWhenHSM",
		hsm.Initial(hsm.Target("foo")),
		hsm.State("foo",
			hsm.Transition(
				hsm.When(func(ctx context.Context, hsm *THSM, event hsm.Event) <-chan struct{} {
					ch := make(chan struct{})
					go func() {
						time.Sleep(1 * time.Millisecond)
						close(ch)
					}()
					return ch
				}),
				hsm.Target("../bar"),
			),
		),
		hsm.State("bar"),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	time.Sleep(4 * time.Millisecond)
	if sm.State() != "/TestWhenHSM/bar" {
		t.Fatalf("expected state to be bar, got %s", sm.State())
	}
}

func TestStop(t *testing.T) {
	sm := hsm.Started(context.Background(), &THSM{}, &benchModel)
	<-hsm.Stop(context.Background(), sm)
	instances, ok := hsm.InstancesFromContext(sm.Context())
	if !ok {
		t.Fatalf("expected instances to be non-nil")
	}
	if len(instances) != 0 {
		t.Fatalf("expected instances to be empty")
	}
}

func TestSnapshot(t *testing.T) {
	sm := hsm.Started(context.Background(), &THSM{}, &benchModel)
	snapshot := hsm.TakeSnapshot(context.Background(), sm)
	if snapshot.ID == "" {
		t.Fatalf("expected snapshot to have an ID")
	}
	if snapshot.State == "" {
		t.Fatalf("expected snapshot to have a state")
	}
	if snapshot.QueueLen != 0 {
		t.Fatalf("expected snapshot to have a queue length of 0")
	}
	if snapshot.QualifiedName == "" {
		t.Fatalf("expected snapshot to have a qualified name")
	}
}

func BenchmarkNonHSM(b *testing.B) {
	handler := nonHSMLogic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !handler(&hsm.Event{
			Name: "foo",
		}) {
			b.Fatal("event not handled")
		}
		if !handler(&hsm.Event{
			Name: "bar",
		}) {
			b.Fatal("event not handled")
		}
	}
}

func BenchmarkHSMWithLargeData(b *testing.B) {
	// Create 1MB of data

	ctx := context.Background()

	instance := hsm.Started(ctx, &THSM{}, &benchModel)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hsm.Dispatch(ctx, instance, hsm.Event{
			Name: "foo",
		})
		hsm.Dispatch(ctx, instance, hsm.Event{
			Name: "bar",
		})

	}
	<-hsm.Stop(ctx, instance)
}

func TestRestart(t *testing.T) {
	model := hsm.Define(
		"TestRestartHSM",
		hsm.Initial(hsm.Target("foo")),
		hsm.State("foo", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
		hsm.Transition(hsm.On(fooEvent), hsm.Source("foo"), hsm.Target("bar")),
		hsm.State("bar", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	if sm.State() != "/TestRestartHSM/foo" {
		t.Fatalf("Expected state to be foo, got: %s", sm.State())
	}
	<-hsm.Dispatch(context.Background(), sm, hsm.Event{Name: "foo"})
	if sm.State() != "/TestRestartHSM/bar" {
		t.Fatalf("Expected state to be bar, got: %s", sm.State())
	}
	hsm.Restart(context.Background(), sm)
	if sm.State() != "/TestRestartHSM/foo" {
		t.Fatalf("Expected state to be foo, got: %s", sm.State())
	}
}

func TestDispatch(t *testing.T) {
	model := hsm.Define(
		"TestDispatchHSM",
		hsm.Initial(hsm.Target("foo")),
		hsm.State("foo", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
		hsm.Transition(hsm.On(fooEvent), hsm.Source("foo"), hsm.Target("bar")),
		hsm.State("bar", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
	)
	sm := hsm.Started(context.Background(), &THSM{}, &model)
	if sm.State() != "/TestDispatchHSM/foo" {
		t.Fatalf("Expected state to be foo, got: %s", sm.State())
	}
	done := hsm.Dispatch(sm.Context(), sm, hsm.Event{Name: "foo"})
	<-done
	if sm.State() != "/TestDispatchHSM/bar" {
		t.Fatalf("Expected state to be bar, got: %s", sm.State())
	}
	<-hsm.Stop(context.Background(), sm)
	select {
	case <-sm.Context().Done():
	default:
		t.Fatalf("Expected state machine to be done")
	}

}

func TestAfter(t *testing.T) {

	sm := hsm.Started(context.Background(), &THSM{}, &benchModel)
	if sm.State() != "/TestHSM/foo" {
		t.Fatalf("Expected state to be foo, got: %s", sm.State())
	}
	entered := hsm.AfterEntry(sm.Context(), sm, "/TestHSM/bar")
	exited := hsm.AfterExit(sm.Context(), sm, "/TestHSM/foo")
	dispatched := hsm.AfterDispatch(sm.Context(), sm, hsm.Event{Name: "foo"})
	processed := hsm.AfterProcess(sm.Context(), sm, hsm.Event{Name: "foo"})

	<-hsm.Dispatch(sm.Context(), sm, hsm.Event{Name: "foo"})
	select {
	case <-dispatched:
	default:
		t.Fatalf("Expected dispatch to be called")
	}
	select {
	case <-entered:
	default:
		t.Fatalf("Expected entry to be called")
	}
	select {
	case <-exited:
	default:
		t.Fatalf("Expected exit to be called")
	}
	select {
	case <-processed:
	default:
		t.Fatalf("Expected processed to be called")
	}

}

func noGuard(ctx context.Context, hsm *THSM, event hsm.Event) bool {
	return true
}

// func TestPropagate(t *testing.T) {
// 	model := hsm.Define(
// 		"TestPropagateHSM",
// 		hsm.Initial(hsm.Target("foo")),
// 		hsm.State("foo", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
// 		hsm.Transition(hsm.On("foo"), hsm.Source("foo"), hsm.Target("bar")),
// 		hsm.State("bar", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
// 	)
// 	sm1 := hsm.Started(context.Background(), &THSM{}, &model)
// 	sm2 := hsm.Started(sm1.Context(), &THSM{}, &model)
// 	<-hsm.Propagate(sm2.Context(), hsm.Event{Name: "foo"})
// 	if sm1.State() != "/TestPropagateHSM/bar" {
// 		t.Fatalf("Expected state to be bar, got: %s", sm1.State())
// 	}
// }

// func TestPropagateAll(t *testing.T) {
// 	model := hsm.Define(
// 		"TestPropagateAllHSM",
// 		hsm.Initial(hsm.Target("foo")),
// 		hsm.State("foo", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
// 		hsm.Transition(hsm.On("foo"), hsm.Source("foo"), hsm.Target("bar")),
// 		hsm.State("bar", hsm.Entry(noBehavior), hsm.Exit(noBehavior)),
// 	)
// 	instances := make([]hsm.Instance, 10)
// 	for i := 0; i < 10; i++ {
// 		var ctx context.Context
// 		if i == 0 {
// 			ctx = context.Background()
// 		} else {
// 			ctx = instances[i-1].Context()
// 		}
// 		instances[i] = hsm.Started(ctx, &THSM{}, &model)
// 	}
// 	<-hsm.PropagateAll(instances[len(instances)-1].Context(), hsm.Event{Name: "foo"})
// 	for i := range len(instances) - 1 {
// 		if instances[i].State() != "/TestPropagateAllHSM/bar" {
// 			t.Fatalf("Expected instance %d state to be bar, got: %s", i, instances[i].State())
// 		}
// 	}
// }

func TestAnyEventTransition(t *testing.T) {
	// Track which events triggered the AnyEvent transition
	var capturedEvents []string
	mutex := &sync.Mutex{}
	doneEvent := hsm.Event{
		Name: "done",
	}
	model := hsm.Define(
		"TestAnyEventHSM",
		hsm.Initial(hsm.Target("idle")),
		hsm.State("idle",
			hsm.Transition(
				hsm.On(hsm.AnyEvent),
				hsm.Target("../processing"),
				hsm.Guard(func(ctx context.Context, sm *THSM, event hsm.Event) bool {
					// Filter out internal HSM events
					switch event.Name {
					case "hsm_initial", "hsm_initialized", "hsm_started", "hsm_completion":
						return false
					}
					return true
				}),
				hsm.Effect(func(ctx context.Context, sm *THSM, event hsm.Event) {
					mutex.Lock()
					capturedEvents = append(capturedEvents, event.Name)
					mutex.Unlock()
				}),
			),
		),
		hsm.State("processing",
			hsm.Entry(func(ctx context.Context, sm *THSM, event hsm.Event) {
				// Automatically return to idle after processing
				hsm.Dispatch(ctx, sm, hsm.Event{
					Name: "done",
					Kind: hsm.CompletionEventKind,
				})
			}),
			hsm.Transition(
				hsm.On(doneEvent),
				hsm.Target("../idle"),
			),
		),
	)

	ctx := context.Background()
	sm := hsm.Started(ctx, &THSM{}, &model)

	// Verify initial state
	if sm.State() != "/TestAnyEventHSM/idle" {
		t.Fatalf("Expected initial state to be idle, got %s", sm.State())
	}

	// Test various event types
	testEvents := []string{"user_input", "system_message", "random_event", "custom_signal"}

	for _, eventName := range testEvents {
		<-hsm.Dispatch(ctx, sm, hsm.Event{Name: eventName})

		// Give time for completion event to process
		time.Sleep(10 * time.Millisecond)

		// Should be back in idle state
		if sm.State() != "/TestAnyEventHSM/idle" {
			t.Fatalf("Expected state to be idle after processing %s, got %s", eventName, sm.State())
		}
	}

	// Verify all events were captured
	mutex.Lock()
	defer mutex.Unlock()

	if len(capturedEvents) != len(testEvents) {
		t.Fatalf("Expected %d captured events, got %d", len(testEvents), len(capturedEvents))
	}

	for i, eventName := range testEvents {
		if capturedEvents[i] != eventName {
			t.Errorf("Expected captured event[%d] to be %s, got %s", i, eventName, capturedEvents[i])
		}
	}
}

func TestAnyEventWithSpecificTransitions(t *testing.T) {
	// Test that specific event transitions take precedence over AnyEvent
	var transitionPath []string
	mutex := &sync.Mutex{}

	recordTransition := func(name string) func(ctx context.Context, sm *THSM, event hsm.Event) {
		return func(ctx context.Context, sm *THSM, event hsm.Event) {
			mutex.Lock()
			transitionPath = append(transitionPath, name)
			mutex.Unlock()
		}
	}
	specialEvent := hsm.Event{
		Name: "special",
	}
	resetEvent := hsm.Event{
		Name: "reset",
	}
	model := hsm.Define(
		"TestAnyEventPriorityHSM",
		hsm.Initial(hsm.Target("ready")),
		hsm.State("ready",
			// Specific transition for "special" event
			hsm.Transition(
				hsm.On(specialEvent),
				hsm.Target("../special_handling"),
				hsm.Effect(recordTransition("special_transition")),
			),
			// Catch-all for any other event
			hsm.Transition(
				hsm.On(hsm.AnyEvent),
				hsm.Target("../general_handling"),
				hsm.Guard(func(ctx context.Context, sm *THSM, event hsm.Event) bool {
					// Filter out internal events
					return !strings.HasPrefix(event.Name, "hsm_")
				}),
				hsm.Effect(recordTransition("any_event_transition")),
			),
		),
		hsm.State("special_handling",
			hsm.Entry(recordTransition("special_handling_entry")),
			hsm.Transition(hsm.On(resetEvent), hsm.Target("../ready")),
		),
		hsm.State("general_handling",
			hsm.Entry(recordTransition("general_handling_entry")),
			hsm.Transition(hsm.On(resetEvent), hsm.Target("../ready")),
		),
	)

	ctx := context.Background()
	sm := hsm.Started(ctx, &THSM{}, &model)

	// Test specific event takes precedence
	<-hsm.Dispatch(ctx, sm, hsm.Event{Name: "special"})
	if sm.State() != "/TestAnyEventPriorityHSM/special_handling" {
		t.Errorf("Expected state to be special_handling, got %s", sm.State())
	}

	// Reset
	<-hsm.Dispatch(ctx, sm, hsm.Event{Name: "reset"})

	// Test general event handling
	<-hsm.Dispatch(ctx, sm, hsm.Event{Name: "anything_else"})
	if sm.State() != "/TestAnyEventPriorityHSM/general_handling" {
		t.Errorf("Expected state to be general_handling, got %s", sm.State())
	}

	// Verify transition path
	mutex.Lock()
	defer mutex.Unlock()

	expectedPath := []string{
		"special_transition",
		"special_handling_entry",
		"any_event_transition",
		"general_handling_entry",
	}

	if len(transitionPath) != len(expectedPath) {
		t.Fatalf("Expected %d transitions, got %d: %v", len(expectedPath), len(transitionPath), transitionPath)
	}

	for i, expected := range expectedPath {
		if transitionPath[i] != expected {
			t.Errorf("Expected transition[%d] to be %s, got %s", i, expected, transitionPath[i])
		}
	}
}

func BenchmarkModel(b *testing.B) {
	iEvent := hsm.Event{
		Name: "i",
	}
	aEvent := hsm.Event{
		Name: "a",
	}
	dEvent := hsm.Event{
		Name: "d",
	}
	gEvent := hsm.Event{
		Name: "g",
	}
	cEvent := hsm.Event{
		Name: "c",
	}
	uEvent := hsm.Event{
		Name: "u",
	}
	xEvent := hsm.Event{
		Name: "x",
	}
	eEvent := hsm.Event{
		Name: "e",
	}
	hEvent := hsm.Event{
		Name: "h",
	}
	jEvent := hsm.Event{
		Name: "j",
	}
	kEvent := hsm.Event{
		Name: "k",
	}
	zEvent := hsm.Event{
		Name: "z",
	}
	for i := 0; i < b.N; i++ {
		_ = hsm.Define(
			"TestHSM",
			hsm.State("s",
				hsm.Entry(noBehavior),
				hsm.Activity(noBehavior),
				hsm.Exit(noBehavior),
				hsm.State("s1",
					hsm.State("s11",
						hsm.Entry(noBehavior),
						hsm.Activity(noBehavior),
						hsm.Exit(noBehavior),
					),
					hsm.Initial(hsm.Target("s11"), hsm.Effect(noBehavior)),
					hsm.Exit(noBehavior),
					hsm.Entry(noBehavior),
					hsm.Activity(noBehavior),
					hsm.Transition(hsm.On(iEvent), hsm.Effect(noBehavior)),
					hsm.Transition(hsm.On(aEvent), hsm.Target("/s/s1"), hsm.Effect(noBehavior)),
				),
				hsm.Transition(hsm.On(dEvent), hsm.Source("/s/s1/s11"), hsm.Target("/s/s1"), hsm.Effect(noBehavior), hsm.Guard(
					noGuard,
				)),
				hsm.Initial(hsm.Target("s1/s11"), hsm.Effect(noBehavior)),
				hsm.State("s2",
					hsm.Entry(noBehavior),
					hsm.Activity(noBehavior),
					hsm.Exit(noBehavior),
					hsm.State("s21",
						hsm.State("s211",
							hsm.Entry(noBehavior),
							hsm.Activity(noBehavior),
							hsm.Exit(noBehavior),
							hsm.Transition(hsm.On(gEvent), hsm.Target("/s/s1/s11"), hsm.Effect(noBehavior)),
						),
						hsm.Initial(hsm.Target("s211"), hsm.Effect(noBehavior)),
						hsm.Entry(noBehavior),
						hsm.Activity(noBehavior),
						hsm.Exit(noBehavior),
						hsm.Transition(hsm.On(aEvent), hsm.Target("/s/s2/s21")), // self transition
					),
					hsm.Initial(hsm.Target("s21/s211"), hsm.Effect(noBehavior)),
					hsm.Transition(hsm.On(cEvent), hsm.Target("/s/s1"), hsm.Effect(noBehavior)),
				),
				hsm.State("s3",
					hsm.Entry(noBehavior),
					hsm.Activity(noBehavior),
					hsm.Exit(noBehavior),
				),
				// Wildcard events are no longer supported
			// hsm.Transition(hsm.On(`*.P.*`), hsm.Effect(noBehavior)),
			),
			hsm.State("t",
				hsm.Entry(noBehavior),
				hsm.Activity(noBehavior),
				hsm.Exit(noBehavior),
				hsm.State(
					"u",
					hsm.Entry(noBehavior),
					hsm.Activity(noBehavior),
					hsm.Exit(noBehavior),
					hsm.Transition(
						hsm.On(uEvent),
						hsm.Target("/t"),
						hsm.Effect(noBehavior),
					),
				),
				hsm.Transition(
					hsm.On(xEvent),
					hsm.Target("/exit"),
					hsm.Effect(noBehavior),
				),
			),

			hsm.Final("exit"),
			hsm.Initial(
				hsm.Target(hsm.Choice(
					"initial_choice",
					hsm.Transition(hsm.Target("/s/s2")),
				)), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(dEvent), hsm.Source("/s/s1"), hsm.Target("/s"), hsm.Effect(noBehavior), hsm.Guard(
				noGuard,
			)),
			// Wildcard events are no longer supported
			// hsm.Transition("wildcard", hsm.On("abcd*"), hsm.Source("/s"), hsm.Target("/s")),
			hsm.Transition(hsm.On(dEvent), hsm.Source("/s"), hsm.Target("/s"), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(cEvent), hsm.Source("/s/s1"), hsm.Target("/s/s2"), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(eEvent), hsm.Source("/s"), hsm.Target("/s/s1/s11"), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(gEvent), hsm.Source("/s/s1/s11"), hsm.Target("/s/s2/s21/s211"), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(iEvent), hsm.Source("/s"), hsm.Effect(noBehavior), hsm.Guard(
				noGuard,
			)),
			hsm.Transition(hsm.After(
				func(ctx context.Context, hsm *THSM, event hsm.Event) time.Duration {
					return time.Second * 2
				},
			), hsm.Source("/s/s2/s21/s211"), hsm.Target("/s/s1/s11"), hsm.Effect(noBehavior), hsm.Guard(
				noGuard,
			)),
			hsm.Transition(hsm.On(hEvent), hsm.Source("/s/s1/s11"), hsm.Target(
				hsm.Choice(
					hsm.Transition(hsm.Target("/s/s1"), hsm.Guard(
						noGuard,
					)),
					hsm.Transition(hsm.Target("/s/s2"), hsm.Effect(noBehavior)),
				),
			), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(jEvent), hsm.Source("/s/s2/s21/s211"), hsm.Target("/s/s1/s11"), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(kEvent), hsm.Source("/s/s1/s11"), hsm.Target("/s/s3"), hsm.Effect(noBehavior)),
			hsm.Transition(hsm.On(zEvent), hsm.Effect(noBehavior), hsm.Source("/s/s3"), hsm.Target("/t/u")),
			hsm.Transition(hsm.On(xEvent), hsm.Effect(noBehavior), hsm.Source("/s/s3"), hsm.Target("/t/u")),
		)
	}
}
