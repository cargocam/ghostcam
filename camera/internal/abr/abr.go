package abr

import (
	"context"
	"log/slog"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/capture"
	"github.com/cargocam/ghostcam/camera/internal/state"
)

// Adaptive bitrate (ABR) controller. Opt-in via cfg.ABREnabled. When
// active, samples outbound video-stream loss every second, ratchets the
// camera's tier up or down per a simple TCP-style state machine, and
// triggers a capture-pipeline restart so rpicam-vid respawns with the
// new --bitrate / --width / --height. Default OFF so existing fleets
// keep their fixed-bitrate behaviour until ABR is field-validated. See
// #52 and docs/design/video-capture.md for the design.
//
// Signal: derived packet-loss ratio from the remote receiver's
// inbound-RTP stats (the loss the viewer actually saw). Local
// BytesSent alone doesn't tell us anything — pion's send buffer
// happily soaks up frames until the encoder catches up.

// DefaultABRTiers is the four-rung ladder from the design doc. Order
// matters: index 0 is the floor, last index is the ceiling. Tweak with
// care — the ABR controller only ever moves one rung at a time.
var DefaultABRTiers = []state.ABRTier{
	{Name: "minimum", Width: 854, Height: 480, Bitrate: 500_000},
	{Name: "low", Width: 854, Height: 480, Bitrate: 1_000_000},
	{Name: "medium", Width: 1280, Height: 720, Bitrate: 2_000_000},
	{Name: "high", Width: 1920, Height: 1080, Bitrate: 4_000_000},
}

// ABRController holds the per-camera ABR state machine. One controller
// per camera daemon; lives for the daemon's lifetime. Reads stats from
// the package-level currentPublisher atomic (in internal/state) so a
// publisher reconnect is observed transparently.
type ABRController struct {
	tiers []state.ABRTier

	// Tunables. The defaults come from #52: fast downgrade (3s of high
	// loss), slow upgrade (15s of clean throughput), 5s cooldown between
	// shifts to keep the bitrate from oscillating around a soft cap.
	highLossRatio float64       // ≥ this triggers downshift candidacy
	lowLossRatio  float64       // ≤ this triggers upshift candidacy
	downSamples   int           // consecutive 1s ticks of high loss for downshift
	upSamples     int           // consecutive 1s ticks of low loss for upshift
	cooldown      time.Duration // refractory period after any shift
	tickInterval  time.Duration

	// State.
	cur          int
	lastChangeAt time.Time
	highRun      int // consecutive ticks above highLossRatio
	lowRun       int // consecutive ticks below lowLossRatio

	// Wiring. Injected so tests can drive the loop deterministically.
	// sampleFn returns the next cumulative stats reading. Production
	// implementation calls Publisher.SampleVideoStats; tests stub it.
	getPublisher  func() *capture.Publisher
	sampleFn      func() (state.VideoSendSample, bool)
	now           func() time.Time
	signalRestart func()

	// Per-tick stat carry. Tracks the previous cumulative sample so
	// the controller can diff to a delta.
	lastSample state.VideoSendSample
	haveLast   bool
	lastPub    *capture.Publisher // detect publisher swap and reset diffs
}

// NewABRController builds a controller pinned to a starting tier
// (default: minimum, matching the "start at minimum tier on cellular"
// guidance from #52 — bumps up only after observed stability).
func NewABRController(start state.ABRTier) *ABRController {
	c := &ABRController{
		tiers:         DefaultABRTiers,
		highLossRatio: 0.05,
		lowLossRatio:  0.01,
		downSamples:   3,
		upSamples:     15,
		cooldown:      5 * time.Second,
		tickInterval:  time.Second,
		cur:           indexOfTier(DefaultABRTiers, start.Name),
		getPublisher: func() *capture.Publisher {
			// state.CurrentPublisher() returns the PublisherHandle
			// interface backed by the active *capture.Publisher.
			// Cast back so the controller's swap-detection logic can
			// keep using pointer equality.
			if h := state.CurrentPublisher(); h != nil {
				if pub, ok := h.(*capture.Publisher); ok {
					return pub
				}
			}
			return nil
		},
		now:           time.Now,
		signalRestart: state.RequestPipelineRestart,
	}
	c.sampleFn = func() (state.VideoSendSample, bool) {
		pub := c.getPublisher()
		if pub == nil {
			return state.VideoSendSample{}, false
		}
		return pub.SampleVideoStats()
	}
	t := c.tiers[c.cur]
	state.SetActiveTier(&t)
	return c
}

// Run blocks until ctx is cancelled, ticking the state machine every
// tickInterval (default 1s). Caller is expected to spawn this in its
// own goroutine alongside the capture loop.
func (c *ABRController) Run(ctx context.Context) {
	tk := time.NewTicker(c.tickInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			c.step()
		}
	}
}

// step advances one tick. Exposed for testing — production code calls
// Run().
func (c *ABRController) step() {
	pub := c.getPublisher()
	if pub == nil {
		// No active publisher (capture restart in flight, or initial
		// connect failed). Drop the run counters so we don't carry a
		// half-formed window into the next session.
		c.resetWindow()
		c.lastPub = nil
		c.haveLast = false
		return
	}
	// Reset diffs if the publisher pointer changed (new WHIP session).
	if pub != c.lastPub {
		c.lastPub = pub
		c.haveLast = false
		c.resetWindow()
	}

	sample, ok := c.sampleFn()
	if !ok || !sample.Fresh {
		// Either no publisher or no recent RR — can't read loss.
		// Drop the windows so a stale signal doesn't bleed into the
		// next decision.
		c.resetWindow()
		c.haveLast = false
		return
	}
	// Detect "publisher idle": SamplesSent didn't move tick-over-tick.
	// Same role the old dPackets <= 0 check served.
	if c.haveLast && sample.SamplesSent == c.lastSample.SamplesSent {
		c.resetWindow()
		return
	}
	c.lastSample = sample
	c.haveLast = true

	loss := sample.LossRatio
	switch {
	case loss >= c.highLossRatio:
		c.highRun++
		c.lowRun = 0
	case loss <= c.lowLossRatio:
		c.lowRun++
		c.highRun = 0
	default:
		// Middle band — neither congested nor clearly idle. Bleed off
		// the upshift counter so we don't promote on a mediocre run,
		// but don't penalise the downshift counter: a downshift fires
		// on sustained high loss only.
		c.lowRun = 0
	}

	now := c.now()
	if now.Sub(c.lastChangeAt) < c.cooldown {
		return
	}

	if c.highRun >= c.downSamples && c.cur > 0 {
		c.shift(c.cur-1, now, "high loss")
		return
	}
	if c.lowRun >= c.upSamples && c.cur < len(c.tiers)-1 {
		c.shift(c.cur+1, now, "stable network")
	}
}

func (c *ABRController) shift(toIdx int, now time.Time, reason string) {
	from := c.tiers[c.cur]
	to := c.tiers[toIdx]
	c.cur = toIdx
	c.lastChangeAt = now
	c.resetWindow()
	tCopy := to
	state.SetActiveTier(&tCopy)
	c.signalRestart()
	slog.Info("ABR tier shift",
		"from", from.Name, "to", to.Name,
		"bitrate_kbps", to.Bitrate/1000,
		"reason", reason)
}

func (c *ABRController) resetWindow() {
	c.highRun = 0
	c.lowRun = 0
}

// ResetToFloor forces the controller back to the minimum tier and
// resets cooldown. Called by network-change hooks (e.g. cellular
// failover) so the camera doesn't try to push 4 Mbps over LTE the
// instant WiFi drops. Safe to call any time; idempotent if already
// at the floor.
func (c *ABRController) ResetToFloor() {
	if c.cur == 0 {
		return
	}
	c.shift(0, c.now(), "network change")
}

func indexOfTier(tiers []state.ABRTier, name string) int {
	for i, t := range tiers {
		if t.Name == name {
			return i
		}
	}
	return 0
}
