//go:build js && wasm

package main

import (
	"regexp"
	"strings"
	"sync"
	"time"

	mosh "github.com/unixshells/mosh-go"
	vt "github.com/unixshells/vt-go"
)

var decModeRe = regexp.MustCompile(`\x1b\[\?([0-9;]+)([hl])`)

// mouseModes are the DEC private modes xterm.js must see to capture pointer input.
var mouseModes = map[string]bool{"9": true, "1000": true, "1002": true, "1003": true, "1005": true, "1006": true, "1015": true, "1016": true}

// stateTracker implements Dart-style received state management.
// Each incoming diff is applied to a copy of the base state (looked up
// by oldNum), producing a new state stored by newNum. The display
// always shows the latest received state, diffed against what's
// currently on screen. This eliminates garbled output from missed
// diffs and overlapping state transitions.
type stateTracker struct {
	mu sync.Mutex

	shadow      *vt.Emulator
	shadowState uint64
	cols, rows  int

	// Received states: stateNum → framebuffer snapshot.
	states       map[uint64]*mosh.Framebuffer
	latestState  uint64
	displayedFB  *mosh.Framebuffer

	// Prediction engine for local echo.
	predictor *mosh.Predictor

	// Output buffer: pre-diffed ANSI ready for the terminal.
	output []byte
}

func newStateTracker(cols, rows int) *stateTracker {
	return &stateTracker{
		shadow:    vt.NewEmulator(cols, rows),
		cols:      cols,
		rows:      rows,
		states:    make(map[uint64]*mosh.Framebuffer),
		predictor: mosh.NewPredictor(),
	}
}

// applyDiff processes an incoming server diff using state tracking.
// throwawayNum is the server's indication of which states it no longer references.
// Returns true if the diff was successfully applied and newNum can be acked.
func (st *stateTracker) applyDiff(diff []byte, oldNum, newNum, throwawayNum uint64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Already have this state — treat as applied.
	if _, ok := st.states[newNum]; ok {
		return true
	}

	// Snapshot current shadow as oldNum if we don't have it.
	if oldNum == st.shadowState {
		if _, ok := st.states[oldNum]; !ok {
			st.states[oldNum] = mosh.SnapshotEmulator(st.shadow, true)
		}
	}

	// Need the base state to apply the diff.
	base, ok := st.states[oldNum]
	if !ok {
		if oldNum != 0 {
			// Base was pruned. Dropping the diff (and not acking it) makes the
			// server re-diff from our last acked state; applying it against a
			// different base would corrupt the screen.
			return false
		}
		st.resetShadow()
		st.shadowState = oldNum
		base = mosh.SnapshotEmulator(st.shadow, true)
		st.states[oldNum] = base
	} else if st.shadowState != oldNum {
		// Restore shadow to the base state.
		st.resetShadow()
		if base != nil {
			st.shadow.Write(base.Diff(nil))
		}
	}

	// Apply diff: feed hoststrings to shadow.
	instrs, err := mosh.UnmarshalHostMessage(diff)
	if err != nil {
		return false
	}
	for _, hi := range instrs {
		if len(hi.Hoststring) > 0 {
			st.shadow.Write(hi.Hoststring)
			// Forward mouse-tracking DEC modes directly to xterm.js output: the
			// shadow emulator's Diff() emits only rendered character cells, so
			// without this xterm.js never activates mouse reporting.
			for _, m := range decModeRe.FindAllSubmatch(hi.Hoststring, -1) {
				for _, p := range strings.Split(string(m[1]), ";") {
					if mouseModes[p] {
						st.output = append(st.output, m[0]...)
						break
					}
				}
			}
		}
	}
	st.shadowState = newNum

	// Snapshot result.
	snap := mosh.SnapshotEmulator(st.shadow, true)
	st.states[newNum] = snap
	if newNum > st.latestState {
		st.latestState = newNum
	}

	// Prune states the server no longer references.
	if throwawayNum > 0 {
		for n := range st.states {
			if n < throwawayNum {
				delete(st.states, n)
			}
		}
	}

	// Hard cap: keep at most 64 states to give the server room to diff against older acks.
	if len(st.states) > 64 {
		minKeep := st.latestState
		if minKeep > 64 {
			minKeep -= 64
		} else {
			minKeep = 0
		}
		for n := range st.states {
			if n < minKeep {
				delete(st.states, n)
			}
		}
	}

	// Confirm predictions against server state.
	latest := st.states[st.latestState]
	if latest != nil {
		st.predictor.Confirm(latest)
		st.predictor.ExpireStale(time.Now())

		// Apply prediction overlay for display.
		displayFB := latest
		if st.predictor.Active() {
			displayFB = st.cloneFB(latest)
			st.predictor.Overlay(displayFB)
		}

		ansi := displayFB.Diff(st.displayedFB)
		if len(ansi) > 0 {
			st.output = append(st.output, ansi...)
		}
		st.displayedFB = displayFB
	}
	return true
}

// has reports whether the diff base oldNum can be reproduced locally.
func (st *stateTracker) has(oldNum uint64) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if oldNum == 0 {
		return true
	}
	_, ok := st.states[oldNum]
	return ok
}

// poll returns accumulated ANSI output, or nil.
func (st *stateTracker) poll() []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.output) == 0 {
		return nil
	}
	out := st.output
	st.output = nil
	return out
}

// redraw forces a complete re-emission of the latest framebuffer to recover
// from any display corruption or blank screens.
func (st *stateTracker) redraw() []byte {
	st.mu.Lock()
	defer st.mu.Unlock()
	latest := st.states[st.latestState]
	if latest == nil {
		if st.displayedFB != nil {
			latest = st.displayedFB
		} else {
			return nil
		}
	}
	st.displayedFB = latest
	return latest.FullRedraw()
}

// keystroke feeds user input to the predictor and returns any prediction
// overlay output that should be displayed immediately.
func (st *stateTracker) keystroke(data []byte) []byte {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.predictor.Keystroke(data)

	if !st.predictor.Active() || st.displayedFB == nil {
		return nil
	}

	// Overlay predictions onto the current display and diff.
	overlaid := st.cloneFB(st.displayedFB)

	// First undo previous prediction overlay by using latest server state.
	var serverFB *mosh.Framebuffer
	if st.latestState > 0 {
		serverFB = st.states[st.latestState]
	}
	if serverFB == nil {
		serverFB = st.displayedFB
	}

	displayFB := st.cloneFB(serverFB)
	st.predictor.Overlay(displayFB)

	ansi := displayFB.Diff(overlaid)
	if len(ansi) > 0 {
		st.displayedFB = displayFB
	}
	return ansi
}

// cloneFB creates a deep copy of a framebuffer.
func (st *stateTracker) cloneFB(fb *mosh.Framebuffer) *mosh.Framebuffer {
	clone := &mosh.Framebuffer{
		W:      fb.W,
		H:      fb.H,
		CurX:   fb.CurX,
		CurY:   fb.CurY,
		CurVis: fb.CurVis,
		Cells:  make([]mosh.Cell, len(fb.Cells)),
	}
	copy(clone.Cells, fb.Cells)
	return clone
}

// resize resets the state tracker for new dimensions.
func (st *stateTracker) resize(cols, rows int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.cols == cols && st.rows == rows {
		return
	}
	st.cols = cols
	st.rows = rows
	// Resize shadow emulator without blowing away existing states
	st.shadow.Resize(cols, rows)
	st.shadowState = 0
	st.states = make(map[uint64]*mosh.Framebuffer)
	st.displayedFB = nil
	st.predictor.Reset()
}

// resetShadow clears the shadow emulator without reallocating it.
func (st *stateTracker) resetShadow() {
	st.shadow.Resize(st.cols, st.rows)
	st.shadow.Write([]byte("\033[H\033[2J\033[m"))
}

