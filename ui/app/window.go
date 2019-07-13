// SPDX-License-Identifier: Unlicense OR MIT

package app

import (
	"errors"
	"fmt"
	"image"
	"sync"
	"time"

	"gioui.org/ui"
	"gioui.org/ui/app/internal/gpu"
	iinput "gioui.org/ui/app/internal/input"
	"gioui.org/ui/input"
	"gioui.org/ui/key"
	"gioui.org/ui/system"
)

type WindowOptions struct {
	Width  ui.Value
	Height ui.Value
	Title  string
}

type Window struct {
	driver     *window
	lastFrame  time.Time
	drawStart  time.Time
	gpu        *gpu.GPU
	inputState key.TextInputState

	events chan Event

	eventLock sync.Mutex

	mu           sync.Mutex
	stage        Stage
	size         image.Point
	syncGPU      bool
	animating    bool
	hasNextFrame bool
	nextFrame    time.Time
	delayedDraw  *time.Timer

	router iinput.Router
}

// driverEvent is sent when a new native driver
// is available for the Window.
type driverEvent struct {
	driver *window
}

// driver is the interface for the platform implementation
// of a Window.
var _ interface {
	// setAnimating sets the animation flag. When the window is animating,
	// DrawEvents are delivered as fast as the display can handle them.
	setAnimating(anim bool)
	// setTextInput updates the virtual keyboard state.
	setTextInput(s key.TextInputState)
} = (*window)(nil)

var ackEvent Event

// NewWindow creates a new window for a set of window
// options. The options are hints; the platform is free to
// ignore or adjust them.
// If the current program is running on iOS and Android,
// NewWindow returns the window previously by the platform.
func NewWindow(opts *WindowOptions) *Window {
	if opts == nil {
		opts = &WindowOptions{
			Width:  ui.Dp(800),
			Height: ui.Dp(600),
			Title:  "Gio program",
		}
	}
	if opts.Width.V <= 0 || opts.Height.V <= 0 {
		panic("window width and height must be larger than 0")
	}

	w := &Window{
		events: make(chan Event),
	}
	if err := createWindow(w, opts); err != nil {
		// For simplicity, NewWindow always succeeds. Send
		// an immediate DestroyEvent instead of returning the error.
		w.destroy(err)
	}
	return w
}

func (w *Window) Events() <-chan Event {
	return w.events
}

func (w *Window) setTextInput(s key.TextInputState) {
	if s != w.inputState && (s == key.TextInputClose || s == key.TextInputOpen) {
		w.driver.setTextInput(s)
	}
	if s == key.TextInputFocus {
		w.setNextFrame(time.Time{})
		w.updateAnimation()
	}
	w.inputState = s
}

func (w *Window) Queue() input.Queue {
	return &w.router
}

func (w *Window) Draw(root *ui.Ops) {
	w.mu.Lock()
	var drawDur time.Duration
	if !w.drawStart.IsZero() {
		drawDur = time.Since(w.drawStart)
		w.drawStart = time.Time{}
	}
	stage := w.stage
	sync := w.syncGPU
	w.syncGPU = false
	alive := w.isAlive()
	size := w.size
	driver := w.driver
	w.mu.Unlock()
	if !alive || stage < StageRunning || driver == nil {
		return
	}
	if w.gpu != nil {
		if sync {
			w.gpu.Refresh()
		}
		if err := w.gpu.Flush(); err != nil {
			w.gpu.Release()
			w.gpu = nil
		}
	}
	if w.gpu == nil {
		ctx, err := newContext(driver)
		if err != nil {
			w.destroy(err)
			return
		}
		w.gpu, err = gpu.NewGPU(ctx)
		if err != nil {
			w.destroy(err)
			return
		}
	}
	w.gpu.Draw(w.router.Profiling(), size, root)
	w.router.Frame(root)
	now := time.Now()
	w.mu.Lock()
	w.setTextInput(w.router.InputState())
	frameDur := now.Sub(w.lastFrame)
	frameDur = frameDur.Truncate(100 * time.Microsecond)
	w.lastFrame = now
	if w.router.Profiling() {
		q := 100 * time.Microsecond
		timings := fmt.Sprintf("tot:%7s cpu:%7s %s", frameDur.Round(q), drawDur.Round(q), w.gpu.Timings())
		w.router.AddProfile(system.ProfileEvent{Timings: timings})
		w.setNextFrame(time.Time{})
	}
	if t, ok := w.router.RedrawTime(); ok {
		w.setNextFrame(t)
	}
	w.updateAnimation()
	w.mu.Unlock()
}

func (w *Window) Redraw() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.isAlive() {
		return
	}
	w.setNextFrame(time.Time{})
	w.updateAnimation()
}

func (w *Window) updateAnimation() {
	animate := false
	if w.delayedDraw != nil {
		w.delayedDraw.Stop()
		w.delayedDraw = nil
	}
	if !w.isAlive() {
		return
	}
	if w.stage >= StageRunning && w.hasNextFrame {
		if dt := time.Until(w.nextFrame); dt <= 0 {
			animate = true
		} else {
			w.delayedDraw = time.AfterFunc(dt, w.Redraw)
		}
	}
	if animate != w.animating {
		w.animating = animate
		w.driver.setAnimating(animate)
	}
}

func (w *Window) setNextFrame(at time.Time) {
	if !w.hasNextFrame || at.Before(w.nextFrame) {
		w.hasNextFrame = true
		w.nextFrame = at
	}
}

func (w *Window) Size() image.Point {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

func (w *Window) Stage() Stage {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stage
}

func (w *Window) isAlive() bool {
	return w.driver != nil
}

func (w *Window) contextDriver() interface{} {
	return w.driver
}

func (w *Window) setDriver(d *window) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.driver = d
}

func (w *Window) destroy(err error) {
	w.setDriver(nil)
	go func() {
		w.event(DestroyEvent{err})
	}()
}

func (w *Window) event(e Event) {
	w.eventLock.Lock()
	defer w.eventLock.Unlock()
	w.mu.Lock()
	died := false
	needAck := false
	switch e := e.(type) {
	case input.Event:
		if w.router.Add(e) {
			w.setNextFrame(time.Time{})
		}
	case *CommandEvent:
		needAck = true
	case DestroyEvent:
		w.driver = nil
		died = true
	case StageEvent:
		w.stage = e.Stage
		needAck = true
		w.syncGPU = true
	case DrawEvent:
		if e.Size == (image.Point{}) {
			panic(errors.New("internal error: zero-sized Draw"))
		}
		if w.stage < StageRunning {
			// No drawing if not visible.
			break
		}
		w.drawStart = time.Now()
		needAck = true
		w.hasNextFrame = false
		w.syncGPU = e.sync
		w.size = e.Size
	}
	stage := w.stage
	w.updateAnimation()
	w.mu.Unlock()
	w.events <- e
	if needAck {
		// Send a dummy event; when it gets through we
		// know the application has processed the actual event.
		w.events <- ackEvent
	}
	if w.gpu != nil {
		w.mu.Lock()
		sync := w.syncGPU
		w.syncGPU = false
		w.mu.Unlock()
		switch {
		case stage < StageRunning:
			w.gpu.Release()
			w.gpu = nil
		case sync:
			w.gpu.Refresh()
		}
	}
	if died {
		close(w.events)
	}
}
