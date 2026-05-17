package main

// WHIP publisher. Reads H.264 Annex-B from one io.Reader (the rpicam-vid /
// ffmpeg video tee) and OGG-encapsulated Opus from another (ffmpeg's audio
// pipe), packetizes both as RTP via pion's media.Sample helpers, and pushes
// a single WHIP session to the server. Uses the pion v4 library, matching
// the server's broadcast hub (see server/whip_ingest.go).
//
// Spike: validated for 2+ minutes of stable 1280×720 / 30 fps + 64 kbps
// Opus on real Pi hardware. See spike/whip-publisher/README.md for the
// design notes and three failed iterations.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// Publisher owns a pion PeerConnection and two media tracks. Lifecycle:
//
//	p := NewPublisher()       // create pc + tracks
//	defer p.Close()
//	p.Connect(ctx, url, sig)  // run WHIP handshake against server
//	p.StreamH264(ctx, vReader) -- goroutine reading NALs into video samples
//	p.StreamOggOpus(ctx, aReader) -- goroutine reading OGG pages into audio
//	... feed bytes into vReader / aReader (typically capture's pipes) ...
//	// on EOF or error, either Stream call returns; publisher stays alive
//	// until p.Close() or pc state changes to Failed/Closed.
type Publisher struct {
	pc           *webrtc.PeerConnection
	videoTrack   *webrtc.TrackLocalStaticSample
	audioTrack   *webrtc.TrackLocalStaticSample
	hasAudio     bool
	connected    chan struct{}
	disconnected chan struct{}

	// Outbound video stats for the ABR controller (#52 / #111). pion
	// v4 doesn't emit OutboundRTPStreamStats from PeerConnection.GetStats,
	// so we collect what we need here at the app boundary:
	//   * videoSamplesSent  — incremented on every WriteSample in
	//                         StreamH264. Lets the controller detect
	//                         "publisher idle / not sending" without
	//                         relying on a stats reading.
	//   * videoFractionLost — receiver-reported loss fraction (0-255,
	//                         from the most recent RTCP Receiver Report
	//                         for the video sender). The state machine
	//                         turns this into a 0..1 ratio.
	//   * videoLastRRAt     — unix nano timestamp of the latest RR.
	//                         Drives the Fresh flag so a stalled or
	//                         missing-RR session doesn't keep tuning
	//                         on stale loss data.
	videoFractionLost atomic.Uint32
	videoLastRRAt     atomic.Int64
	videoSamplesSent  atomic.Uint64
}

// Disconnected returns a channel closed when the WHIP peer connection
// transitions to Failed or Closed. Callers — typically the capture pipeline
// supervisor — watch this alongside ctx.Done() so that a server-side drop
// (deploy, viewer churn, network blip) tears down the pipeline and lets the
// outer reconnect loop spin up a fresh publisher. Without this, the
// publisher silently zombifies: ffmpeg keeps pumping bytes into a closed
// pc and no new WHIP session is ever negotiated.
func (p *Publisher) Disconnected() <-chan struct{} { return p.disconnected }

// NewPublisher creates the peer connection and tracks. Connect() must be
// called separately to perform the WHIP HTTP handshake.
//
// withAudio: when false, no audio track is added — used for video-only
// captures (cfg.NoAudio).
func NewPublisher(withAudio bool) (*Publisher, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "ghostcam-publisher",
	)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("new video track: %w", err)
	}
	if _, err := pc.AddTrack(videoTrack); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add video track: %w", err)
	}

	var audioTrack *webrtc.TrackLocalStaticSample
	if withAudio {
		audioTrack, err = webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
			"audio", "ghostcam-publisher",
		)
		if err != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("new audio track: %w", err)
		}
		if _, err := pc.AddTrack(audioTrack); err != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("add audio track: %w", err)
		}
	}

	p := &Publisher{
		pc:           pc,
		videoTrack:   videoTrack,
		audioTrack:   audioTrack,
		hasAudio:     withAudio,
		connected:    make(chan struct{}),
		disconnected: make(chan struct{}),
	}

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		slog.Info("publisher pc state", "state", s.String())
		switch s {
		case webrtc.PeerConnectionStateConnected:
			select {
			case <-p.connected:
			default:
				close(p.connected)
			}
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			select {
			case <-p.disconnected:
			default:
				close(p.disconnected)
			}
		}
	})

	// Drain RTCP — PLIs from viewers, Receiver Reports, etc. RRs feed
	// the ABR controller's loss measurement via videoFractionLost +
	// videoLastRRAt. PLIs are still discarded (rpicam-vid --intra
	// already emits periodic keyframes); future work would forward
	// PLI to a "force keyframe" channel.
	for _, sender := range pc.GetSenders() {
		go p.drainSenderRTCP(sender)
	}

	return p, nil
}

// Connect performs the WHIP HTTP handshake against serverURL. bearer is
// optional; when set it's sent as `Authorization: Bearer <bearer>`. In real
// ghostcam this is the ed25519 signature carrier (see spike/whip-whep
// checkCameraSignature for the wire format we'll standardise on).
func (p *Publisher) Connect(ctx context.Context, serverURL, bearer string) error {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return ctx.Err()
	}

	answerSDP, err := postWHIPOffer(ctx, serverURL, bearer, p.pc.LocalDescription().SDP)
	if err != nil {
		return fmt.Errorf("WHIP POST: %w", err)
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answerSDP,
	}); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	slog.Info("WHIP session established", "server", serverURL)

	// Wait briefly for ICE/DTLS to settle. Not strictly required — sending
	// samples before connected just buffers them — but lets callers know
	// the session is healthy before they begin pumping media.
	select {
	case <-p.connected:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("WHIP session did not reach Connected within 10s")
	}
	return nil
}

// StreamH264 reads Annex-B NAL units from r, accumulates them into access
// units, and emits one media.Sample per AU. Returns when r returns EOF or
// when ctx is cancelled.
//
// AU batching is required: pion's H264 packetizer sets the RTP marker bit
// on the last packet of each Sample, and the H.264-over-RTP spec says
// marker=1 means "end of access unit." Sending parameter-set NALs as their
// own samples makes browsers drop the real frames.
//
// AU boundary: a slice NAL starts a new AU only when it is the FIRST slice
// of a new picture, detected via first_mb_in_slice == 0 in the slice
// header. Multi-slice frames (e.g. libx264 with sliced-threads enabled, or
// any encoder configured for 2+ slices per picture) emit several VCL NALs
// per picture; collapsing them by NAL unit type alone would drop every
// slice after the first, leaving the receiver to discard frames it can't
// fully reassemble. See #104 for the regression that motivated this.
func (p *Publisher) StreamH264(ctx context.Context, r io.Reader) error {
	return streamH264(ctx, r, func(s media.Sample) error {
		if err := p.videoTrack.WriteSample(s); err != nil {
			return err
		}
		// Bump the ABR controller's "the publisher is still
		// alive and producing frames" counter. Atomic, lock-free —
		// the controller polls this once a second.
		p.videoSamplesSent.Add(1)
		return nil
	})
}

// streamH264 is the testable inner loop. emit is invoked once per
// access unit; production code wires it to TrackLocalStaticSample.
// Tests pass a collector to inspect the AU boundaries directly.
func streamH264(ctx context.Context, r io.Reader, emit func(media.Sample) error) error {
	reader, err := h264reader.NewReader(r)
	if err != nil {
		return fmt.Errorf("h264 reader: %w", err)
	}
	const targetFPS = 30
	frameDur := time.Second / targetFPS
	startCode := []byte{0x00, 0x00, 0x00, 0x01}

	var (
		au       []byte
		hasSlice bool
		frames   uint64
	)
	flush := func() error {
		if len(au) == 0 {
			return nil
		}
		// Copy because the next NAL append may grow/reslice the underlying
		// array while pion's packetizer is still reading the previous bytes.
		buf := make([]byte, len(au))
		copy(buf, au)
		if err := emit(media.Sample{Data: buf, Duration: frameDur}); err != nil {
			return err
		}
		frames++
		if frames%150 == 0 {
			slog.Debug("publisher video", "frames_sent", frames)
		}
		au = au[:0]
		hasSlice = false
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		nal, err := reader.NextNAL()
		if errors.Is(err, io.EOF) {
			return flush()
		}
		if err != nil {
			return err
		}
		slice := isSliceNAL(nal.UnitType)
		// First slice of a new picture closes out the previous AU.
		// Subsequent slices of the same picture (first_mb_in_slice > 0)
		// extend the current AU instead.
		if slice && hasSlice && isFirstSliceOfPicture(nal.Data) {
			if err := flush(); err != nil {
				return err
			}
		}
		au = append(au, startCode...)
		au = append(au, nal.Data...)
		if slice {
			hasSlice = true
		}
	}
}

// isSliceNAL reports whether a NAL unit type carries coded macroblock
// data (VCL). Slice data partitions A/B/C (types 2-4) are accepted along
// with the ordinary slice types so the boundary check stays correct for
// partitioned bitstreams, even though x264 doesn't emit them by default.
func isSliceNAL(t h264reader.NalUnitType) bool {
	switch t {
	case h264reader.NalUnitTypeCodedSliceIdr,
		h264reader.NalUnitTypeCodedSliceNonIdr,
		h264reader.NalUnitTypeCodedSliceDataPartitionA,
		h264reader.NalUnitTypeCodedSliceDataPartitionB,
		h264reader.NalUnitTypeCodedSliceDataPartitionC:
		return true
	}
	return false
}

// isFirstSliceOfPicture returns true when a slice NAL's slice_header
// declares first_mb_in_slice == 0 — i.e. this slice covers the top-left
// macroblock and therefore starts a new coded picture. Subsequent slices
// of the same picture have first_mb_in_slice > 0.
//
// data is the NAL unit payload as emitted by h264reader (no start-code
// prefix). Byte 0 is the NAL unit header; byte 1 onwards is the RBSP.
// first_mb_in_slice is the first ue(v) value in the slice header; value 0
// is encoded as the single bit '1', so the MSB of data[1] is set iff
// first_mb_in_slice == 0. Emulation-prevention bytes never appear at
// this position (they only follow two zero bytes), so a direct MSB test
// is safe.
func isFirstSliceOfPicture(data []byte) bool {
	if len(data) < 2 {
		return true
	}
	return data[1]&0x80 != 0
}

// StreamOggOpus reads an OGG-encapsulated Opus stream and emits each Opus
// packet as a media.Sample. ffmpeg must be invoked with
// `-page_duration 20000` so each OGG page carries one 20 ms Opus packet —
// otherwise pion's Opus packetizer can't depacketize.
func (p *Publisher) StreamOggOpus(ctx context.Context, r io.Reader) error {
	if p.audioTrack == nil {
		// Audio disabled. Drain the reader so callers don't deadlock on
		// pipe writes.
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	ogg, _, err := oggreader.NewWith(r)
	if err != nil {
		return fmt.Errorf("ogg reader: %w", err)
	}
	var lastGranule uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pageData, pageHeader, err := ogg.ParseNextPage()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parse ogg page: %w", err)
		}
		samples := pageHeader.GranulePosition - lastGranule
		lastGranule = pageHeader.GranulePosition
		dur := time.Duration(samples) * time.Second / 48000
		if dur <= 0 {
			// OpusHead and OpusTags pages have granule 0.
			continue
		}
		if err := p.audioTrack.WriteSample(media.Sample{Data: pageData, Duration: dur}); err != nil {
			return fmt.Errorf("write audio sample: %w", err)
		}
	}
}

// Close shuts down the peer connection.
func (p *Publisher) Close() error {
	return p.pc.Close()
}

// VideoSendSample is a snapshot of the publisher's outbound video state
// for the ABR controller. LossRatio is the receiver-reported fraction
// (0..1) from the most recent RTCP Receiver Report; SamplesSent is the
// cumulative count of access units the publisher has written. Fresh
// is true iff an RR arrived recently enough that LossRatio reflects
// the current network — the controller refuses to tune on stale data.
//
// Pre-#111 this struct exposed pion's OutboundRTPStreamStats fields
// (BytesSent/PacketsSent/PacketsLost), but pion v4 never populates
// that stats type from GetStats. The new shape reads the atomics
// the publisher maintains directly. The state machine is unchanged.
type VideoSendSample struct {
	LossRatio   float64
	SamplesSent uint64
	Fresh       bool
}

// rrFreshness is how long after the most recent Receiver Report the
// publisher will keep treating LossRatio as current. ~3 RR intervals
// at pion's default RR cadence (~5 s) so a brief skipped RR doesn't
// stall the controller, but a genuine RR drop quickly does.
const rrFreshness = 15 * time.Second

// SampleVideoStats returns the publisher's current outbound video
// state. Always returns ok=true once the publisher exists — Fresh on
// the returned sample tells the controller whether to trust the
// LossRatio. Pre-first-RR returns Fresh=false; long network silence
// also flips back to Fresh=false.
func (p *Publisher) SampleVideoStats() (VideoSendSample, bool) {
	if p == nil || p.pc == nil {
		return VideoSendSample{}, false
	}
	lastRRNs := p.videoLastRRAt.Load()
	out := VideoSendSample{SamplesSent: p.videoSamplesSent.Load()}
	if lastRRNs == 0 {
		return out, true
	}
	if time.Since(time.Unix(0, lastRRNs)) > rrFreshness {
		return out, true
	}
	out.LossRatio = float64(p.videoFractionLost.Load()) / 256.0
	out.Fresh = true
	return out, true
}

// CaptureSink is the live-stream attachment point for the capture
// pipeline. h264Writer receives raw Annex-B from the rpicam-vid tee;
// audioWriter receives ffmpeg's OGG-Opus output. cleanup must be
// called when the capture session ends so the publisher's reader
// goroutines see EOF and exit.
type CaptureSink struct {
	H264Writer  io.Writer
	AudioWriter io.Writer
	cleanup     func()
}

// Close releases the writer ends, causing publisher.StreamH264 /
// StreamOggOpus goroutines to return EOF and finish.
func (s *CaptureSink) Close() { s.cleanup() }

// NewCaptureSink wires the capture pipeline to the publisher's media
// readers via in-process io.Pipes. When pub is nil, both writers are
// io.Discard and no goroutines start — used when live publishing is
// disabled (no server configured).
func NewCaptureSink(ctx context.Context, pub *Publisher) *CaptureSink {
	if pub == nil {
		return &CaptureSink{
			H264Writer:  io.Discard,
			AudioWriter: io.Discard,
			cleanup:     func() {},
		}
	}
	h264R, h264W := io.Pipe()
	audioR, audioW := io.Pipe()

	go func() {
		if err := pub.StreamH264(ctx, h264R); err != nil &&
			!errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.Canceled) {
			slog.Debug("publisher h264 stream ended", "err", err)
		}
		_ = h264R.Close()
	}()
	go func() {
		if err := pub.StreamOggOpus(ctx, audioR); err != nil &&
			!errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, context.Canceled) {
			slog.Debug("publisher opus stream ended", "err", err)
		}
		_ = audioR.Close()
	}()
	return &CaptureSink{
		H264Writer:  h264W,
		AudioWriter: audioW,
		cleanup: func() {
			_ = h264W.Close()
			_ = audioW.Close()
		},
	}
}

// drainSenderRTCP reads RTCP packets bound for one sender. Always
// drains so pion's internal queues don't back up. For the video
// sender it also unmarshals ReceiverReports and snapshots the
// reported FractionLost into the publisher's atomics so the ABR
// controller has something to look at — see #111 for why this is
// done here rather than via PeerConnection.GetStats().
func (p *Publisher) drainSenderRTCP(sender *webrtc.RTPSender) {
	track := sender.Track()
	isVideo := track != nil && track.Kind() == webrtc.RTPCodecTypeVideo
	buf := make([]byte, 1500)
	firstRR := true
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		if !isVideo {
			continue
		}
		pkts, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			slog.Debug("rtcp unmarshal failed", "bytes", n, "err", err)
			continue
		}
		for _, pkt := range pkts {
			rr, ok := pkt.(*rtcp.ReceiverReport)
			if !ok {
				continue
			}
			// All ReceptionReports on a video sender's RTCP socket
			// describe the video stream; the first one is enough.
			if len(rr.Reports) == 0 {
				continue
			}
			p.videoFractionLost.Store(uint32(rr.Reports[0].FractionLost))
			p.videoLastRRAt.Store(time.Now().UnixNano())
			if firstRR {
				// One-shot confirmation that the RR channel is open.
				// After this, ABR shifts are the only thing worth
				// logging — per-RR is noise.
				slog.Info("first video RR received",
					"fraction_lost", rr.Reports[0].FractionLost,
					"total_lost", rr.Reports[0].TotalLost)
				firstRR = false
			}
		}
	}
}

func postWHIPOffer(ctx context.Context, serverURL, bearer, offer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(offer))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/sdp")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("WHIP server returned %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}
