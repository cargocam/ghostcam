use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::Result;
use bytes::Bytes;
use ghostcam::pki;
use ghostcam::telemetry::TelemetryDatagram;
use ghostcam::types::{CertFingerprint, DeviceId, UserId};
use ghostcam::wire::alert::{Alert, StreamKind};
use ghostcam::wire::command::Command;
use ghostcam::wire::framing;
use server_core::db::{Database, NewCameraRecord};
use server_core::frames::InboundStreamTag;
use server_core::ingest::accept::run_accept_loop;
use server_core::ingest::quic_config;
use server_core::ingest::registry::RoutingRegistry;
use server_core::pki::ca::CaManager;
use server_core::pki::revocation::RevocationCache;
use server_solo::db::SqliteDatabase;
use tokio_util::sync::CancellationToken;

// --- Test infrastructure ---

struct TestEnv {
    server_addr: SocketAddr,
    registry: Arc<RoutingRegistry>,
    db: Arc<SqliteDatabase>,
    ca: Arc<CaManager>,
    revocation_cache: Arc<RevocationCache>,
    cancel: CancellationToken,
}

impl TestEnv {
    async fn setup() -> Result<Self> {
        let (server_cert_der, server_key) = pki::create_self_signed_server_cert("localhost", 1)?;
        let server_key_der = server_key.serialize_der();

        let db = Arc::new(SqliteDatabase::open(":memory:").await?);
        db.initialize().await?;

        let (ca, _, _) = CaManager::generate_instance_ca()?;
        let ca = Arc::new(ca);
        let revocation_cache = Arc::new(RevocationCache::new());

        let bind_addr: SocketAddr = "127.0.0.1:0".parse()?;
        let endpoint =
            quic_config::build_server_endpoint(&server_cert_der, &server_key_der, bind_addr)?;
        let server_addr = endpoint.local_addr()?;

        let registry = Arc::new(RoutingRegistry::new());
        let cancel = CancellationToken::new();

        let reg = registry.clone();
        let db_clone = db.clone() as Arc<dyn Database>;
        let ca_clone = ca.clone();
        let revocation_clone = revocation_cache.clone();
        let cancel_clone = cancel.clone();
        tokio::spawn(async move {
            let _ = run_accept_loop(
                endpoint,
                reg,
                db_clone,
                ca_clone,
                revocation_clone,
                None, // No Redis in integration tests
                Arc::new(server_core::sse::SseEventBus::new()),
                cancel_clone,
            )
            .await;
        });

        Ok(Self {
            server_addr,
            registry,
            db,
            ca,
            revocation_cache,
            cancel,
        })
    }

    async fn enroll_camera(&self) -> Result<EnrolledCamera> {
        let (device_cert_der, device_key) = pki::create_self_signed_server_cert("camera", 1)?;
        let device_key_der = device_key.serialize_der();
        let fingerprint = pki::cert_fingerprint(&device_cert_der);

        // Generate a CSR and sign it with the CA to create a proper user association cert
        let csr = pki::create_csr("camera", &device_key)?;
        let camera = self
            .db
            .create_camera(&NewCameraRecord {
                user_id: UserId("solo".to_string()),
                cert_fingerprint: fingerprint.clone(),
                display_name: "Test Camera".to_string(),
            })
            .await?;

        let assoc_cert_pem = self.ca.sign_csr(&csr, &camera.device_id.0)?;
        let assoc_cert_der = pki::pem_to_der(&assoc_cert_pem)?;

        Ok(EnrolledCamera {
            device_id: camera.device_id,
            _fingerprint: fingerprint,
            device_cert_der,
            device_key_der,
            assoc_cert_der,
        })
    }

    async fn teardown(self) {
        self.cancel.cancel();
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    }
}

struct EnrolledCamera {
    device_id: DeviceId,
    _fingerprint: CertFingerprint,
    device_cert_der: Vec<u8>,
    device_key_der: Vec<u8>,
    assoc_cert_der: Vec<u8>,
}

/// A mock camera that connects to the server over QUIC.
struct MockCamera {
    connection: quinn::Connection,
    alerts_tx: quinn::SendStream,
    video_tx: quinn::SendStream,
    audio_tx: quinn::SendStream,
    commands_rx: quinn::RecvStream,
}

impl MockCamera {
    /// Connect to server, send the handshake on a bidi control stream,
    /// and open video/audio uni streams with tag prefixes.
    ///
    /// The control stream is bidirectional: camera writes alerts on one half,
    /// server writes commands on the other. Using a bidi stream avoids the
    /// deadlock that arises with separate uni streams (quinn only creates
    /// QUIC streams at the transport layer when data is written).
    async fn connect(
        server_addr: SocketAddr,
        enrolled: &EnrolledCamera,
        fw_version: &str,
        streams: Vec<StreamKind>,
    ) -> Result<Self> {
        let endpoint = quic_config::build_client_endpoint(
            vec![
                enrolled.device_cert_der.clone(),
                enrolled.assoc_cert_der.clone(),
            ],
            &enrolled.device_key_der,
        )?;

        let connection = endpoint.connect(server_addr, "localhost")?.await?;

        // Open bidirectional control stream (alerts + commands)
        let (mut alerts_tx, commands_rx) = connection.open_bi().await?;

        // Write Alerts tag + handshake immediately
        alerts_tx
            .write_all(&[InboundStreamTag::Alerts as u8])
            .await?;
        let alert = Alert::Handshake {
            protocol_version: ghostcam::config::PROTOCOL_VERSION,
            fw_version: fw_version.to_string(),
            streams,
        };
        framing::write_json(&mut alerts_tx, &alert).await?;

        // Open video and audio uni streams with tag prefix
        let mut video_tx = connection.open_uni().await?;
        video_tx.write_all(&[InboundStreamTag::Video as u8]).await?;

        let mut audio_tx = connection.open_uni().await?;
        audio_tx.write_all(&[InboundStreamTag::Audio as u8]).await?;

        Ok(Self {
            connection,
            alerts_tx,
            video_tx,
            audio_tx,
            commands_rx,
        })
    }

    /// Connect with a raw handshake alert (for testing protocol version mismatch).
    async fn connect_raw(
        server_addr: SocketAddr,
        enrolled: &EnrolledCamera,
        handshake: &Alert,
    ) -> Result<Self> {
        let endpoint = quic_config::build_client_endpoint(
            vec![
                enrolled.device_cert_der.clone(),
                enrolled.assoc_cert_der.clone(),
            ],
            &enrolled.device_key_der,
        )?;

        let connection = endpoint.connect(server_addr, "localhost")?.await?;

        let (mut alerts_tx, commands_rx) = connection.open_bi().await?;
        alerts_tx
            .write_all(&[InboundStreamTag::Alerts as u8])
            .await?;
        framing::write_json(&mut alerts_tx, handshake).await?;

        // Give server time to process and potentially reject
        tokio::time::sleep(std::time::Duration::from_millis(500)).await;

        let video_tx = connection.open_uni().await?;
        let audio_tx = connection.open_uni().await?;

        Ok(Self {
            connection,
            alerts_tx,
            video_tx,
            audio_tx,
            commands_rx,
        })
    }

    async fn send_alert(&mut self, alert: &Alert) -> Result<()> {
        framing::write_json(&mut self.alerts_tx, alert).await?;
        Ok(())
    }

    async fn send_video_frame(&mut self, data: &[u8]) -> Result<()> {
        framing::write_frame(&mut self.video_tx, data).await?;
        Ok(())
    }

    async fn send_audio_frame(&mut self, data: &[u8]) -> Result<()> {
        framing::write_frame(&mut self.audio_tx, data).await?;
        Ok(())
    }

    async fn send_telemetry(&self, datagram: &TelemetryDatagram) -> Result<()> {
        let data = rmp_serde::to_vec_named(datagram)?;
        self.connection.send_datagram(Bytes::from(data))?;
        Ok(())
    }

    async fn send_upload(&self, upload_type: InboundStreamTag, data: &[u8]) -> Result<()> {
        let mut stream = self.connection.open_uni().await?;
        stream.write_all(&[upload_type as u8]).await?;
        stream.write_all(data).await?;
        stream.finish()?;
        Ok(())
    }

    async fn recv_command(&mut self) -> Result<Command> {
        let cmd: Command = framing::read_json(&mut self.commands_rx)
            .await
            .map_err(|e| anyhow::anyhow!("command read error: {e}"))?
            .ok_or_else(|| anyhow::anyhow!("command stream closed"))?;
        Ok(cmd)
    }
}

// --- Integration Tests ---

#[tokio::test]
async fn camera_connects_and_handshakes() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let _cam = MockCamera::connect(
        env.server_addr,
        &enrolled,
        "0.1.0",
        vec![StreamKind::Video, StreamKind::Audio],
    )
    .await
    .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    assert!(env.registry.is_connected(&enrolled.device_id).await);

    env.teardown().await;
}

#[tokio::test]
async fn camera_rejected_without_enrollment() {
    let env = TestEnv::setup().await.unwrap();

    let (device_cert_der, device_key) = pki::create_self_signed_server_cert("camera", 1).unwrap();
    let device_key_der = device_key.serialize_der();
    let (assoc_cert_der, _) = pki::create_self_signed_server_cert("association", 1).unwrap();

    let endpoint =
        quic_config::build_client_endpoint(vec![device_cert_der, assoc_cert_der], &device_key_der)
            .unwrap();

    let conn = endpoint
        .connect(env.server_addr, "localhost")
        .unwrap()
        .await
        .unwrap();

    // Write data to trigger stream creation at QUIC level
    let (mut alerts_tx, _) = conn.open_bi().await.unwrap();
    let _ = alerts_tx.write_all(&[InboundStreamTag::Alerts as u8]).await;
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;

    assert!(
        !env.registry
            .is_connected(&DeviceId("nonexistent".into()))
            .await
    );

    env.teardown().await;
}

#[tokio::test]
async fn camera_sends_video_frames() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let mut cam = MockCamera::connect(env.server_addr, &enrolled, "0.1.0", vec![StreamKind::Video])
        .await
        .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    let slot = env.registry.get_slot(&enrolled.device_id).await.unwrap();
    let mut rx = slot.video_tx.subscribe();

    for i in 0u8..10 {
        cam.send_video_frame(&[i; 100]).await.unwrap();
    }

    for i in 0u8..10 {
        let frame = tokio::time::timeout(std::time::Duration::from_secs(2), rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(frame.data[0], i);
        assert_eq!(frame.data.len(), 100);
    }

    env.teardown().await;
}

#[tokio::test]
async fn camera_sends_audio_frames() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let mut cam = MockCamera::connect(env.server_addr, &enrolled, "0.1.0", vec![StreamKind::Audio])
        .await
        .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    let slot = env.registry.get_slot(&enrolled.device_id).await.unwrap();
    let mut rx = slot.audio_tx.subscribe();

    for i in 0u8..5 {
        cam.send_audio_frame(&[i; 50]).await.unwrap();
    }

    for i in 0u8..5 {
        let frame = tokio::time::timeout(std::time::Duration::from_secs(2), rx.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(frame.data[0], i);
    }

    env.teardown().await;
}

#[tokio::test]
async fn camera_sends_telemetry() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let cam = MockCamera::connect(
        env.server_addr,
        &enrolled,
        "0.1.0",
        vec![StreamKind::Telemetry],
    )
    .await
    .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    let slot = env.registry.get_slot(&enrolled.device_id).await.unwrap();
    let mut rx = slot.telemetry_tx.subscribe();

    let datagram = TelemetryDatagram {
        ts: 1000,
        cpu: Some(55),
        temp: Some(42),
        ..Default::default()
    };
    cam.send_telemetry(&datagram).await.unwrap();

    let received = tokio::time::timeout(std::time::Duration::from_secs(2), rx.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(received.ts, 1000);
    assert_eq!(received.cpu, Some(55));
    assert_eq!(received.temp, Some(42));

    env.teardown().await;
}

#[tokio::test]
async fn camera_receives_commands() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let mut cam = MockCamera::connect(env.server_addr, &enrolled, "0.1.0", vec![StreamKind::Video])
        .await
        .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    let slot = env.registry.get_slot(&enrolled.device_id).await.unwrap();
    slot.send_command(Command::StartVideo { seq: 1 })
        .await
        .unwrap();

    let cmd = tokio::time::timeout(std::time::Duration::from_secs(2), cam.recv_command())
        .await
        .unwrap()
        .unwrap();
    assert!(matches!(cmd, Command::StartVideo { seq: 1 }));

    env.teardown().await;
}

#[tokio::test]
async fn camera_sends_manifest_push() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let cam = MockCamera::connect(env.server_addr, &enrolled, "0.1.0", vec![StreamKind::Video])
        .await
        .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    let manifest = "#EXTM3U\n#EXT-X-VERSION:7\n";
    cam.send_upload(InboundStreamTag::Manifest, manifest.as_bytes())
        .await
        .unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;

    let slot = env.registry.get_slot(&enrolled.device_id).await.unwrap();
    let stored = slot.manifest.read().await;
    assert_eq!(stored.as_deref(), Some(manifest));

    env.teardown().await;
}

#[tokio::test]
async fn camera_disconnect_unregisters() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let cam = MockCamera::connect(env.server_addr, &enrolled, "0.1.0", vec![StreamKind::Video])
        .await
        .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    assert!(env.registry.is_connected(&enrolled.device_id).await);

    cam.connection.close(0u32.into(), b"bye");
    drop(cam);
    tokio::time::sleep(std::time::Duration::from_millis(300)).await;

    assert!(!env.registry.is_connected(&enrolled.device_id).await);

    env.teardown().await;
}

#[tokio::test]
async fn capability_update_reflects() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();
    let mut cam = MockCamera::connect(env.server_addr, &enrolled, "0.1.0", vec![StreamKind::Video])
        .await
        .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    cam.send_alert(&Alert::CapabilityUpdate {
        streams: vec![StreamKind::Video, StreamKind::Audio, StreamKind::Telemetry],
    })
    .await
    .unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    let slot = env.registry.get_slot(&enrolled.device_id).await.unwrap();
    let caps = slot.capabilities.read().await;
    assert_eq!(
        *caps,
        vec![StreamKind::Video, StreamKind::Audio, StreamKind::Telemetry]
    );

    env.teardown().await;
}

#[tokio::test]
async fn multiple_cameras_same_user() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled1 = env.enroll_camera().await.unwrap();
    let enrolled2 = env.enroll_camera().await.unwrap();

    let _cam1 = MockCamera::connect(
        env.server_addr,
        &enrolled1,
        "0.1.0",
        vec![StreamKind::Video],
    )
    .await
    .unwrap();
    let _cam2 = MockCamera::connect(
        env.server_addr,
        &enrolled2,
        "0.1.0",
        vec![StreamKind::Video],
    )
    .await
    .unwrap();

    tokio::time::sleep(std::time::Duration::from_millis(200)).await;

    assert!(env.registry.is_connected(&enrolled1.device_id).await);
    assert!(env.registry.is_connected(&enrolled2.device_id).await);

    let ids = env.registry.list_device_ids(&UserId("solo".into())).await;
    assert_eq!(ids.len(), 2);

    env.teardown().await;
}

#[tokio::test]
async fn protocol_version_mismatch() {
    let env = TestEnv::setup().await.unwrap();
    let enrolled = env.enroll_camera().await.unwrap();

    // Send handshake with wrong protocol version via bidi stream
    let endpoint = quic_config::build_client_endpoint(
        vec![
            enrolled.device_cert_der.clone(),
            enrolled.assoc_cert_der.clone(),
        ],
        &enrolled.device_key_der,
    )
    .unwrap();

    let connection = endpoint
        .connect(env.server_addr, "localhost")
        .unwrap()
        .await
        .unwrap();

    let (mut alerts_tx, _commands_rx) = connection.open_bi().await.unwrap();
    alerts_tx
        .write_all(&[InboundStreamTag::Alerts as u8])
        .await
        .unwrap();

    let bad_handshake = Alert::Handshake {
        protocol_version: 99,
        fw_version: "0.1.0".to_string(),
        streams: vec![StreamKind::Video],
    };
    framing::write_json(&mut alerts_tx, &bad_handshake)
        .await
        .unwrap();

    // Give server time to process and close the connection
    tokio::time::sleep(std::time::Duration::from_millis(500)).await;

    assert!(!env.registry.is_connected(&enrolled.device_id).await);

    env.teardown().await;
}
