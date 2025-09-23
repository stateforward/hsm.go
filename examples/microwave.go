package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"github.com/stateforward/hsm"
)

// Microwave Instance struct
type Microwave struct {
	hsm.HSM
	time           int
	power          int // Default 50% power
	doorOpen       bool
	lightOn        bool
	heating        bool
	displayMessage string
	timerInterval  *time.Ticker
	stopChan       chan struct{} // Channel to signal stopping activities
	mu             sync.Mutex    // Mutex to protect Microwave state
}

func NewMicrowave() *Microwave {
	m := &Microwave{
		power:    50, // Default 50% power
		stopChan: make(chan struct{}),
	}
	return m
}

func (m *Microwave) SetTime(seconds int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.time = seconds
}

func (m *Microwave) AddTime(seconds int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.time += seconds
	if m.time > 5999 {
		m.time = 5999 // Max 99:59
	}
}

func (m *Microwave) SetPower(level int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.power = max(10, min(100, level))
}

func (m *Microwave) GetTimeDisplay() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	minutes := m.time / 60
	seconds := m.time % 60
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

// State machine behavior methods
func (m *Microwave) onIdleEntry(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.displayMessage = "Ready"
	m.lightOn = false
	m.heating = false
}

func (m *Microwave) canSetTime(ctx context.Context, _ *Microwave, event hsm.Event) bool {
	data, ok := event.Data.(map[string]interface{})
	if !ok {
		return false
	}
	seconds, ok := data["seconds"].(int)
	return ok && seconds > 0
}

func (m *Microwave) setTimeEffect(ctx context.Context, _ *Microwave, event hsm.Event) {
	data, ok := event.Data.(map[string]interface{})
	if !ok {
		return
	}
	seconds, ok := data["seconds"].(int)
	if ok {
		m.SetTime(seconds)
	}
}

func (m *Microwave) addThirtySecondsEffect(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.AddTime(30)
}

func (m *Microwave) setPowerEffect(ctx context.Context, _ *Microwave, event hsm.Event) {
	data, ok := event.Data.(map[string]interface{})
	if !ok {
		return
	}
	level, ok := data["level"].(int)
	if ok {
		m.SetPower(level)
	}
}

func (m *Microwave) onDoorOpenEntry(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.doorOpen = true
	m.lightOn = true
	m.displayMessage = "Door Open"
}

func (m *Microwave) onDoorOpenExit(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.doorOpen = false
}

func (m *Microwave) onReadyEntry(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.displayMessage = "Press Start"
}

func (m *Microwave) clearTimeEffect(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.time = 0
}

func (m *Microwave) onCookingEntry(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lightOn = true
	m.heating = true
	m.displayMessage = "Cooking"
}

func (m *Microwave) onCookingExit(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heating = false
}

func (m *Microwave) cookingActivity(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	if m.timerInterval != nil {
		m.timerInterval.Stop()
	}
	m.timerInterval = time.NewTicker(1 * time.Second)
	m.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			m.timerInterval.Stop()
			m.timerInterval = nil
			m.mu.Unlock()
			return
		case <-m.timerInterval.C:
			m.mu.Lock()
			m.time--
			if m.time <= 0 {
				m.timerInterval.Stop()
				m.timerInterval = nil
				m.mu.Unlock() // Unlock before dispatching to prevent deadlock
				m.Dispatch(ctx, hsm.Event{Name: "timer_complete"})
				return
			}
			m.mu.Unlock()
		}
	}
}

func (m *Microwave) onPausedEntry(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.displayMessage = "Paused"
	m.lightOn = true
	m.doorOpen = true
}

func (m *Microwave) onPausedExit(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.doorOpen = false
}

func (m *Microwave) onPausedReadyEntry(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.displayMessage = "Press Start to Resume"
}

func (m *Microwave) onCompleteEntry(ctx context.Context, _ *Microwave, event hsm.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.displayMessage = "Complete!"
	m.heating = false
}

func (m *Microwave) completeBeepActivity(ctx context.Context, _ *Microwave, event hsm.Event) {
	beepCount := 0
	beepInterval := time.NewTicker(500 * time.Millisecond)
	defer beepInterval.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-beepInterval.C:
			if beepCount >= 3 {
				return
			}
			m.mu.Lock()
			if beepCount%2 == 0 {
				m.displayMessage = "BEEP!"
			} else {
				m.displayMessage = "Complete!"
			}
			m.mu.Unlock()
			beepCount++
		}
	}
}

func (m *Microwave) getCompleteTimeout(ctx context.Context, _ *Microwave, event hsm.Event) time.Duration {
	return 3 * time.Second
}

// UI functions
func (m *Microwave) updateDisplay() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clear screen
	fmt.Print("\033[H\033[2J")

	// Calculate time display string directly to avoid deadlock
	minutes := m.time / 60
	seconds := m.time % 60
	timeDisplay := fmt.Sprintf("%02d:%02d", minutes, seconds)
	
	// Get current state for display
	currentStateName := m.State()
	if currentStateName == "" {
		currentStateName = "Unknown"
	} else {
		currentStateName = path.Base(currentStateName)
	}

	// Door character - changes based on door state
	doorChar := "|"
	if m.doorOpen {
		doorChar = ">"
	}

	// Light indicator
	lightChar := " "
	if m.lightOn {
		lightChar = "*"
	}

	// Heating indicator
	heatingChar := " "
	if m.heating {
		heatingChar = "~"
	}

	fmt.Println("        ╔═══════════════════════════════════════════════════════════╗")
	fmt.Printf("        ║  %s %s %s    M I C R O W A V E    O V E N    %s %s %s        ║\n", lightChar, lightChar, lightChar, lightChar, lightChar, lightChar)
	fmt.Println("        ║                                                           ║")
	fmt.Printf("        ║    ╔═══════════════════════════════╗     ╔═══════════╗    ║\n")
	fmt.Printf("        ║    ║                               ║     ║           ║    ║\n")
	fmt.Printf("        ║    ║        %s %s %s %s %s %s %s %s        ║     ║   %s   ║    ║\n", heatingChar, heatingChar, heatingChar, heatingChar, heatingChar, heatingChar, heatingChar, heatingChar, timeDisplay)
	fmt.Printf("        ║    ║                               ║     ║           ║    ║\n")
	fmt.Printf("        ║    ║                               ║     ║ %s ║    ║\n", m.displayMessage)
	fmt.Printf("        ║    ║         I N T E R I O R       ║     ║  PWR:%3d%% ║    ║\n", m.power)
	fmt.Printf("        ║    ║                               ║     ╚═══════════╝    ║\n")
	fmt.Printf("        ║    ║          C H A M B E R        ║                      ║\n")
	fmt.Printf("        ║    ║                               ║     ╔═════════════╗  ║\n")
	fmt.Printf("        ║    ║                               ║     ║ ┌───┬───┬───┐ ║  ║\n")
	fmt.Printf("        ║    ║                               ║     ║ │ 7 │ 8 │ 9 │ ║  ║\n")
	fmt.Printf("        ║    ║                               ║     ║ ├───┼───┼───┤ ║  ║\n")
	fmt.Printf("        ║    ║                               ║     ║ │ 4 │ 5 │ 6 │ ║  ║\n")
	fmt.Printf("        ║    ╚═══════════════════════════════╝     ║ ├───┼───┼───┤ ║  ║\n")
	fmt.Printf("       %s║                                         ║ │ 1 │ 2 │ 3 │ ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║ ├───┼───┼───┤ ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║ │ 0 │   │CLR│ ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║ └───┴───┴───┘ ║  ║\n", doorChar)
	fmt.Printf("       %s║      ████  DOOR HANDLE  ████            ╠═════════════╣  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  ┌─────────┐  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  │ S T A R T │  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  └─────────┘  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  ┌─────────┐  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  │ S T O P │  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  └─────────┘  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  ┌─────────┐  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  │ +30 SEC │  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  └─────────┘  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  ┌─────────┐  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  │ P O W E R │  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ║  └─────────┘  ║  ║\n", doorChar)
	fmt.Printf("       %s║                                         ╚═════════════╝  ║\n", doorChar)
	fmt.Println("        ╚═══════════════════════════════════════════════════════════╝")
	fmt.Printf("\n    Status: %s | State: %s\n", m.displayMessage, currentStateName)
	
	// Status indicators
	indicators := []string{}
	if m.doorOpen {
		indicators = append(indicators, "DOOR OPEN")
	}
	if m.lightOn {
		indicators = append(indicators, "LIGHT ON")
	}
	if m.heating {
		indicators = append(indicators, "HEATING")
	}
	if len(indicators) > 0 {
		fmt.Printf("    Active: %s\n", fmt.Sprintf("%v", indicators))
	}
}

func (m *Microwave) quit() {
	close(m.stopChan)
	os.Exit(0)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// main function to run the microwave simulator
func main() {
	mw := NewMicrowave()
	model := mw.DefineMicrowaveModel()
	ctx, cancel := context.WithCancel(context.Background())
	sm := hsm.Start(ctx, mw, &model)

	// Initial state
	mw.updateDisplay()

	// Simulate some events
	time.Sleep(1 * time.Second)
	sm.Dispatch(ctx, hsm.Event{Name: "time_set", Data: map[string]interface{}{"seconds": 5}})
	mw.updateDisplay()

	time.Sleep(1 * time.Second)
	sm.Dispatch(ctx, hsm.Event{Name: "start"})
	mw.updateDisplay()

	// Let it cook for a bit
	time.Sleep(3 * time.Second)
	mw.updateDisplay()

	// Open the door to pause
	sm.Dispatch(ctx, hsm.Event{Name: "door_open"})
	mw.updateDisplay()

	time.Sleep(1 * time.Second)

	// Close the door
	sm.Dispatch(ctx, hsm.Event{Name: "door_close"})
	mw.updateDisplay()

	time.Sleep(1 * time.Second)

	// Start again
	sm.Dispatch(ctx, hsm.Event{Name: "start"})
	mw.updateDisplay()

	// Wait for it to finish
	time.Sleep(4 * time.Second) // A bit longer to see the complete state
	mw.updateDisplay()

	hsm.Stop(ctx, sm)
	cancel()
}

// DefineMicrowaveModel defines the state machine model for the Microwave.
func (m *Microwave) DefineMicrowaveModel() hsm.Model {
	return hsm.Define("Microwave",
		hsm.Initial(hsm.Target("idle")),

		hsm.State("idle",
			hsm.Entry(m.onIdleEntry),
			hsm.Transition(
				hsm.On("door_open"),
				hsm.Target("../doorOpen"),
			),
			hsm.Transition(
				hsm.On("time_set"),
				hsm.Guard(m.canSetTime),
				hsm.Target("../ready"),
				hsm.Effect(m.setTimeEffect),
			),
			hsm.Transition(
				hsm.On("add_30s"),
				hsm.Target("../cooking"),
				hsm.Effect(m.addThirtySecondsEffect),
			),
			hsm.Transition(
				hsm.On("power_set"),
				hsm.Effect(m.setPowerEffect),
			),
		),

		hsm.State("doorOpen",
			hsm.Entry(m.onDoorOpenEntry),
			hsm.Exit(m.onDoorOpenExit),
			hsm.Transition(
				hsm.On("door_close"),
				hsm.Target("../idle"),
			),
		),

		hsm.State("ready",
			hsm.Entry(m.onReadyEntry),
			hsm.Transition(
				hsm.On("start"),
				hsm.Target("../cooking"),
			),
			hsm.Transition(
				hsm.On("clear"),
				hsm.Target("../idle"),
				hsm.Effect(m.clearTimeEffect),
			),
			hsm.Transition(
				hsm.On("door_open"),
				hsm.Target("../doorOpen"),
			),
			hsm.Transition(
				hsm.On("power_set"),
				hsm.Effect(m.setPowerEffect),
			),
			hsm.Transition(
				hsm.On("add_30s"),
				hsm.Effect(m.addThirtySecondsEffect),
			),
		),

		hsm.State("cooking",
			hsm.Entry(m.onCookingEntry),
			hsm.Exit(m.onCookingExit),
			hsm.Activity(m.cookingActivity),
			hsm.Transition(
				hsm.On("door_open"),
				hsm.Target("../paused"),
			),
			hsm.Transition(
				hsm.On("stop"),
				hsm.Target("../idle"),
				hsm.Effect(m.clearTimeEffect),
			),
			hsm.Transition(
				hsm.On("add_30s"),
				hsm.Effect(m.addThirtySecondsEffect),
			),
			hsm.Transition(
				hsm.On("timer_complete"),
				hsm.Target("../complete"),
			),
		),

		hsm.State("paused",
			hsm.Entry(m.onPausedEntry),
			hsm.Exit(m.onPausedExit),
			hsm.Transition(
				hsm.On("door_close"),
				hsm.Target("../pausedReady"),
			),
			hsm.Transition(
				hsm.On("clear"),
				hsm.Target("../idle"),
				hsm.Effect(m.clearTimeEffect),
			),
			hsm.Transition(
				hsm.On("power_set"),
				hsm.Effect(m.setPowerEffect),
			),
		),

		hsm.State("pausedReady",
			hsm.Entry(m.onPausedReadyEntry),
			hsm.Transition(
				hsm.On("start"),
				hsm.Target("../cooking"),
			),
			hsm.Transition(
				hsm.On("clear"),
				hsm.Target("../idle"),
				hsm.Effect(m.clearTimeEffect),
			),
			hsm.Transition(
				hsm.On("door_open"),
				hsm.Target("../paused"),
			),
			hsm.Transition(
				hsm.On("power_set"),
				hsm.Effect(m.setPowerEffect),
			),
		),

		hsm.State("complete",
			hsm.Entry(m.onCompleteEntry),
			hsm.Activity(m.completeBeepActivity),
			hsm.Transition(
				hsm.On("door_open"),
				hsm.Target("../doorOpen"),
			),
			hsm.Transition(
				hsm.On("clear"),
				hsm.Target("../idle"),
				hsm.Effect(m.clearTimeEffect),
			),
			hsm.Transition(
				hsm.After(m.getCompleteTimeout),
				hsm.Target("../idle"),
				hsm.Effect(m.clearTimeEffect),
			),
		),
	)
}