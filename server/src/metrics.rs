use prometheus_client::encoding::text::encode;
use prometheus_client::metrics::counter::Counter;
use prometheus_client::metrics::gauge::Gauge;
use prometheus_client::registry::Registry;
use std::sync::atomic::AtomicI64;

pub struct Metrics {
    pub registry: Registry,
    // Gauges
    pub active_cameras: Gauge<i64, AtomicI64>,
    pub active_sessions: Gauge<i64, AtomicI64>,
    // Counters
    pub camera_connections_total: Counter,
    pub camera_disconnections_total: Counter,
    pub viewer_sessions_total: Counter,
    pub auth_successes_total: Counter,
    pub auth_failures_total: Counter,
    pub video_frames_total: Counter,
    pub audio_frames_total: Counter,
    pub video_bytes_total: Counter,
    pub audio_bytes_total: Counter,
}

impl Metrics {
    pub fn new() -> Self {
        let mut registry = Registry::default();

        let active_cameras = Gauge::<i64, AtomicI64>::default();
        let active_sessions = Gauge::<i64, AtomicI64>::default();
        let camera_connections_total = Counter::default();
        let camera_disconnections_total = Counter::default();
        let viewer_sessions_total = Counter::default();
        let auth_successes_total = Counter::default();
        let auth_failures_total = Counter::default();
        let video_frames_total = Counter::default();
        let audio_frames_total = Counter::default();
        let video_bytes_total = Counter::default();
        let audio_bytes_total = Counter::default();

        registry.register("ghostcam_active_cameras", "Number of connected cameras", active_cameras.clone());
        registry.register("ghostcam_active_sessions", "Number of active viewer sessions", active_sessions.clone());
        registry.register("ghostcam_camera_connections_total", "Total camera connections", camera_connections_total.clone());
        registry.register("ghostcam_camera_disconnections_total", "Total camera disconnections", camera_disconnections_total.clone());
        registry.register("ghostcam_viewer_sessions_total", "Total viewer sessions created", viewer_sessions_total.clone());
        registry.register("ghostcam_auth_successes_total", "Total successful auth attempts", auth_successes_total.clone());
        registry.register("ghostcam_auth_failures_total", "Total failed auth attempts", auth_failures_total.clone());
        registry.register("ghostcam_video_frames_total", "Total video frames received", video_frames_total.clone());
        registry.register("ghostcam_audio_frames_total", "Total audio frames received", audio_frames_total.clone());
        registry.register("ghostcam_video_bytes_total", "Total video bytes received", video_bytes_total.clone());
        registry.register("ghostcam_audio_bytes_total", "Total audio bytes received", audio_bytes_total.clone());

        Self {
            registry,
            active_cameras,
            active_sessions,
            camera_connections_total,
            camera_disconnections_total,
            viewer_sessions_total,
            auth_successes_total,
            auth_failures_total,
            video_frames_total,
            audio_frames_total,
            video_bytes_total,
            audio_bytes_total,
        }
    }

    pub fn encode(&self) -> String {
        let mut buf = String::new();
        encode(&mut buf, &self.registry).unwrap();
        buf
    }
}
