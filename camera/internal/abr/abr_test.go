package abr

import (
	"testing"
	"time"

	"github.com/cargocam/ghostcam/camera/internal/capture"
	"github.com/cargocam/ghostcam/camera/internal/state"
)

// ABR state-machine tests. The controller's Run() pulls live stats off
// an actual Publisher, which we can't construct without a peer
// connection. Tests drive the controller through its step()
// abstraction instead, using a scripted sequence of (packets, lost)
// counter snapshots in place of pion's GetStats() output. This isolates
// the threshold / cooldown / hysteresis logic from the WebRTC plumbing,
// which is exactly the part field-tuning will revisit.

// scriptedPublisher is a Publisher stand-in that returns scripted
// VideoSendSample readings. The controller reads the absolute
// LossRatio and the SamplesSent counter; the test feeds both via the
// helper.
type scriptedPublisher struct {
	step    int
	samples []state.VideoSendSample
}

// Sample returns the next scripted reading. Pre-#111 the second
// return value was "do we have stats" with the sample empty when
// none were available; post-#111 the publisher always returns a
// sample and signals "no fresh data" via the Fresh field, so we
// mirror that here: real samples come back with ok=true, and only
// "ran out of script" returns ok=false.
func (s *scriptedPublisher) Sample() (state.VideoSendSample, bool) {
	if s.step >= len(s.samples) {
		return state.VideoSendSample{}, false
	}
	v := s.samples[s.step]
	s.step++
	return v, true
}

// makeController builds an ABRController wired to a scripted publisher
// and a fake clock advanced by tick=1s. Returns the controller plus a
// pointer to the current "now" so the test can move time forward.
func makeController(start string, samples []state.VideoSendSample) (*ABRController, *time.Time, *bool) {
	now := time.Unix(0, 0)
	restartFired := false

	// Build a Publisher-compatible shim. We can't fabricate a real
	// *capture.Publisher (it owns a pion PC) so we route through a
	// wrapper that hands back VideoSendSample via a sample-source the
	// controller can call.
	sp := &scriptedPublisher{samples: samples}
	c := &ABRController{
		tiers:         DefaultABRTiers,
		highLossRatio: 0.05,
		lowLossRatio:  0.01,
		downSamples:   3,
		upSamples:     15,
		cooldown:      5 * time.Second,
		tickInterval:  time.Second,
		cur:           indexOfTier(DefaultABRTiers, start),
		now:           func() time.Time { return now },
		signalRestart: func() { restartFired = true },
	}
	t := c.tiers[c.cur]
	state.SetActiveTier(&t)

	// Bridge the scripted publisher into the controller. We can't pass
	// it as a *capture.Publisher because the type differs, so we hand-roll
	// a step() driver that mirrors the production loop but pulls from sp
	// directly. Keeps the production code path under test by reusing
	// the same shift / cooldown rules.
	c.getPublisher = func() *capture.Publisher { return placeholderPublisher }

	// Replace the per-tick sample fetcher via a closure: drive step()
	// manually and have it consume from the scripted source.
	c.sampleFn = sp.Sample

	return c, &now, &restartFired
}

// placeholderPublisher is a non-nil sentinel the test uses so the
// controller's "publisher present" check passes without us building a
// real pion PC. The controller only dereferences it when SampleVideoStats
// runs, which the test stubs via sampleFn.
var placeholderPublisher = &capture.Publisher{}

// Helper: build a fresh sample with growing SamplesSent and a fixed
// loss ratio so each test data row stays compact.
func freshSample(samplesSent uint64, loss float64) state.VideoSendSample {
	return state.VideoSendSample{LossRatio: loss, SamplesSent: samplesSent, Fresh: true}
}

func TestABRDownshiftOnHighLoss(t *testing.T) {
	// Three consecutive high-loss ticks (≥5%) should drop us one rung.
	// SamplesSent must grow tick-over-tick so the "publisher idle"
	// guard doesn't fire.
	samples := []state.VideoSendSample{
		freshSample(30, 0.10),
		freshSample(60, 0.10),
		freshSample(90, 0.10),
	}
	c, now, restart := makeController("medium", samples)
	*now = now.Add(10 * time.Second) // past cooldown.

	if c.cur != 2 {
		t.Fatalf("start at medium, got cur=%d", c.cur)
	}

	for i := 0; i < 3; i++ {
		c.step()
		*now = now.Add(time.Second)
	}

	if c.cur != 1 {
		t.Errorf("expected downshift to low (idx 1), got cur=%d", c.cur)
	}
	if !*restart {
		t.Errorf("expected signalRestart to fire on tier shift")
	}
	if got := state.ActiveTier(); got == nil || got.Name != "low" {
		t.Errorf("activeTier not updated; got %+v", got)
	}
}

func TestABRUpshiftOnSustainedCleanNetwork(t *testing.T) {
	// 15 clean ticks (loss == 0) should bump us one rung.
	samples := []state.VideoSendSample{}
	for i := uint64(1); i <= 15; i++ {
		samples = append(samples, freshSample(i*30, 0))
	}
	c, now, restart := makeController("minimum", samples)
	*now = now.Add(10 * time.Second)

	if c.cur != 0 {
		t.Fatalf("start at minimum, got cur=%d", c.cur)
	}

	for i := 0; i < 15; i++ {
		c.step()
		*now = now.Add(time.Second)
	}

	if c.cur != 1 {
		t.Errorf("expected upshift to low (idx 1), got cur=%d", c.cur)
	}
	if !*restart {
		t.Errorf("expected signalRestart to fire on tier shift")
	}
}

func TestABRCooldownPreventsRapidOscillation(t *testing.T) {
	// High loss for 3 ticks → downshift. Immediately followed by 3 more
	// high-loss ticks within the 5s cooldown → no second shift.
	samples := []state.VideoSendSample{
		freshSample(30, 0.10),
		freshSample(60, 0.10),
		freshSample(90, 0.10),  // first downshift fires here
		freshSample(120, 0.10), // still in cooldown
		freshSample(150, 0.10), // still in cooldown
		freshSample(180, 0.10), // still in cooldown
	}
	c, now, _ := makeController("high", samples)
	*now = now.Add(10 * time.Second)

	for i := 0; i < 3; i++ {
		c.step()
		*now = now.Add(time.Second)
	}
	if c.cur != 2 {
		t.Fatalf("expected first downshift to medium (idx 2), got cur=%d", c.cur)
	}

	for i := 0; i < 3; i++ {
		c.step()
		*now = now.Add(time.Second)
	}
	if c.cur != 2 {
		t.Errorf("expected cooldown to hold tier at medium (idx 2), got cur=%d", c.cur)
	}
}

func TestABRNeverGoesBelowFloorOrAboveCeiling(t *testing.T) {
	// At minimum, sustained high loss should not produce a sub-floor shift.
	samples := []state.VideoSendSample{}
	for i := uint64(1); i <= 20; i++ {
		samples = append(samples, freshSample(i*30, 0.10))
	}
	c, now, _ := makeController("minimum", samples)
	*now = now.Add(10 * time.Second)
	for i := 0; i < 6; i++ {
		c.step()
		*now = now.Add(time.Second)
	}
	if c.cur != 0 {
		t.Errorf("floor breached: cur=%d", c.cur)
	}

	// At high, sustained clean network should not produce a super-ceiling shift.
	samples = nil
	for i := uint64(1); i <= 25; i++ {
		samples = append(samples, freshSample(i*30, 0))
	}
	c, now, _ = makeController("high", samples)
	*now = now.Add(10 * time.Second)
	for i := 0; i < 22; i++ {
		c.step()
		*now = now.Add(time.Second)
	}
	if c.cur != 3 {
		t.Errorf("ceiling breached: cur=%d", c.cur)
	}
}

// === End-to-end tests for ABR plumbing ====================================
//
// The state-machine tests above stub out the package atomics; these tests
// drive the REAL activeTier + requestPipelineRestart atomics and run a
// fake capture loop that mirrors main.go's restart-watching behaviour.
// This exercises the wire from "controller decides" through "capture
// notices the flag" to "next pipeline spawn reads the new tier" — the
// path that has to work for any rpicam-vid restart to actually happen.

// resetABRGlobals zeroes the package-level atomics ABR writes. Must be
// called from a t.Cleanup so tests don't contaminate each other when
// the package-level test runner picks them up sequentially.
func resetABRGlobals(t *testing.T) {
	t.Helper()
	state.SetActiveTier(nil)
	state.ResetPipelineRestart()
	state.SetCurrentPublisher(nil)
}

func TestABREndToEnd_ShiftReachesCaptureLoop(t *testing.T) {
	resetABRGlobals(t)
	t.Cleanup(func() { resetABRGlobals(t) })

	// Build a controller that uses the REAL package atomics for
	// activeTier and requestPipelineRestart. Only `now` and the
	// stats source are stubbed.
	now := time.Unix(0, 0).Add(10 * time.Second)
	c := NewABRController(state.ABRTier{Name: "medium"})
	c.now = func() time.Time { return now }
	c.tickInterval = time.Millisecond // not used (we drive step() manually)
	// Hand-rolled scripted stats: three consecutive high-loss ticks.
	sp := &scriptedPublisher{samples: []state.VideoSendSample{
		freshSample(30, 0.10),
		freshSample(60, 0.10),
		freshSample(90, 0.10),
	}}
	c.getPublisher = func() *capture.Publisher { return placeholderPublisher }
	c.sampleFn = sp.Sample
	// NewABRController seeded activeTier with the start tier — confirm
	// before the shift so the assertion afterwards is meaningful.
	if got := state.ActiveTier(); got == nil || got.Name != "medium" {
		t.Fatalf("initial activeTier should be medium, got %+v", got)
	}

	// Fake capture loop: mirrors main.go's restart-watching pattern.
	// Loops forever; each iteration reads activeTier (the value
	// rpicam-vid would have been spawned with) and then blocks until
	// requestPipelineRestart fires. Reports each "spawn" to a channel
	// so the test can assert what tier the spawn used.
	type spawn struct {
		iter int
		tier state.ABRTier
	}
	spawns := make(chan spawn, 8)
	stop := make(chan struct{})
	go func() {
		iter := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			t := state.ActiveTier()
			if t == nil {
				time.Sleep(time.Millisecond)
				continue
			}
			spawns <- spawn{iter: iter, tier: *t}
			iter++
			// Wait for the restart signal. Poll the CAS so a single
			// restart triggers exactly one new spawn iteration, just
			// like main.go's `CompareAndSwap(true, false)` watcher.
			for {
				select {
				case <-stop:
					return
				default:
				}
				if state.ConsumePipelineRestart() {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Drain the initial spawn (capture had started at "medium").
	select {
	case s := <-spawns:
		if s.tier.Name != "medium" {
			t.Fatalf("initial spawn at %q, want medium", s.tier.Name)
		}
	case <-time.After(time.Second):
		close(stop)
		t.Fatal("initial capture spawn never fired")
	}

	// Drive three high-loss ticks: the new design doesn't need a
	// priming tick because step() uses the absolute LossRatio, not a
	// dLost/dPackets diff. Cooldown (5s) has already elapsed since
	// lastChangeAt=zero + 10s offset.
	for i := 0; i < 3; i++ {
		c.step()
		now = now.Add(time.Second)
	}

	// Expect the controller to have shifted to "low" and the fake
	// capture loop to have observed the restart and re-spawned with
	// the new tier.
	select {
	case s := <-spawns:
		if s.tier.Name != "low" {
			t.Errorf("post-shift spawn at %q, want low", s.tier.Name)
		}
		if s.tier.Bitrate != 1_000_000 {
			t.Errorf("post-shift bitrate %d, want 1_000_000", s.tier.Bitrate)
		}
	case <-time.After(time.Second):
		t.Fatal("capture loop never observed restart flag — ABR→capture wire is broken")
	}

	close(stop)
	// Drain anything still in-flight so the goroutine can exit cleanly.
	for {
		select {
		case <-spawns:
		default:
			return
		}
	}
}

func TestABREndToEnd_PublisherSwapResetsDiffs(t *testing.T) {
	resetABRGlobals(t)
	t.Cleanup(func() { resetABRGlobals(t) })

	// Two publishers: A produces a SamplesSent counter that grows over
	// time. Then the camera reconnects (pubB) — its counter restarts
	// at a much lower value. Without the publisher-swap reset in
	// step(), the haveLast check would see SamplesSent decrease and
	// either be confused by the idle-detection rule or spuriously
	// trigger. Pin the actual behaviour: a swap drops the carry and
	// rebuilds the baseline from scratch.
	pubA := &capture.Publisher{}
	pubB := &capture.Publisher{}

	now := time.Unix(0, 0).Add(10 * time.Second)
	c := NewABRController(state.ABRTier{Name: "medium"})
	c.now = func() time.Time { return now }

	currentPub := pubA
	c.getPublisher = func() *capture.Publisher { return currentPub }

	// Scripted samples: 4 ticks against pubA (clean), then 2 against
	// pubB whose counter restarts much lower.
	samples := []state.VideoSendSample{
		freshSample(30, 0),
		freshSample(60, 0),
		freshSample(90, 0),
		freshSample(120, 0),
		// Swap → pubB; counters reset.
		freshSample(10, 0),
		freshSample(20, 0),
	}
	idx := 0
	c.sampleFn = func() (state.VideoSendSample, bool) {
		if idx >= len(samples) {
			return state.VideoSendSample{}, false
		}
		v := samples[idx]
		idx++
		return v, true
	}

	// 4 ticks with pubA — clean network, well under upSamples=15.
	for i := 0; i < 4; i++ {
		c.step()
		now = now.Add(time.Second)
	}
	if c.cur != 2 {
		t.Fatalf("unexpected tier shift before publisher swap: cur=%d", c.cur)
	}

	// Swap publisher pointer. step() should notice and discard the
	// prior baseline rather than reading the swapped-down counter as
	// "publisher went idle".
	currentPub = pubB

	c.step()
	now = now.Add(time.Second)
	if c.cur != 2 {
		t.Errorf("publisher swap produced spurious tier change: cur=%d", c.cur)
	}
	c.step()
	if c.cur != 2 {
		t.Errorf("tier changed unexpectedly after baseline rebuild: cur=%d", c.cur)
	}
	if c.lastPub != pubB {
		t.Errorf("lastPub not updated to new publisher")
	}
}

// TestResolveCaptureVideoParams moved to internal/capture/capture_test.go
// alongside its subject. See refactor/camera-subpackages PR notes.

func TestABRResetToFloor(t *testing.T) {
	c, _, restart := makeController("high", []state.VideoSendSample{{}})
	*restart = false
	if c.cur != 3 {
		t.Fatalf("start at high, got cur=%d", c.cur)
	}
	c.ResetToFloor()
	if c.cur != 0 {
		t.Errorf("expected ResetToFloor → minimum (idx 0), got cur=%d", c.cur)
	}
	if !*restart {
		t.Errorf("expected restart to fire on ResetToFloor")
	}
	if got := state.ActiveTier(); got == nil || got.Name != "minimum" {
		t.Errorf("activeTier not updated after reset; got %+v", got)
	}
}
