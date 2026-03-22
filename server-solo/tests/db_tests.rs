use ghostcam::types::{CertFingerprint, DeviceId, SessionId, TokenId, UserId};
use server_core::auth;
use server_core::db::{
    CameraUpdate, Database, NewApiToken, NewCameraRecord, NewEnrollmentToken, NewSession,
};
use server_solo::db::SqliteDatabase;

async fn fresh_db() -> SqliteDatabase {
    SqliteDatabase::open(":memory:").await.unwrap()
}

fn solo_user() -> UserId {
    UserId("solo".to_string())
}

// --- Initialization ---

#[tokio::test]
async fn first_run_creates_owner() {
    let db = fresh_db().await;
    let result = db.initialize().await.unwrap();
    assert!(result.is_some(), "first run should return initial password");
}

#[tokio::test]
async fn first_run_returns_initial_password() {
    let db = fresh_db().await;
    let pw = db.initialize().await.unwrap().unwrap();
    assert_eq!(pw.len(), 16);
    assert!(pw.chars().all(|c| c.is_ascii_alphanumeric()));
}

#[tokio::test]
async fn second_run_skips_init() {
    let db = fresh_db().await;
    db.initialize().await.unwrap();
    let second = db.initialize().await.unwrap();
    assert!(
        second.is_none(),
        "second run should not regenerate password"
    );
}

#[tokio::test]
async fn first_run_creates_hmac_secret() {
    let db = fresh_db().await;
    db.initialize().await.unwrap();
    let secret = db.get_hmac_secret().await.unwrap();
    assert_eq!(secret.len(), 32);
}

#[tokio::test]
async fn health_check_succeeds() {
    let db = fresh_db().await;
    db.health_check().await.unwrap();
}

#[tokio::test]
async fn migrate_is_idempotent() {
    let db = fresh_db().await;
    db.migrate().await.unwrap();
    db.migrate().await.unwrap();
}

// --- Camera CRUD ---

fn new_camera(fingerprint: &str) -> NewCameraRecord {
    NewCameraRecord {
        user_id: solo_user(),
        cert_fingerprint: CertFingerprint(fingerprint.to_string()),
        display_name: "New Camera".to_string(),
    }
}

#[tokio::test]
async fn create_camera_assigns_uuid() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-1")).await.unwrap();
    assert!(uuid::Uuid::parse_str(&cam.device_id.0).is_ok());
}

#[tokio::test]
async fn create_camera_stores_fingerprint() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-2")).await.unwrap();
    let found = db
        .get_camera_by_fingerprint(&CertFingerprint("fp-2".to_string()))
        .await
        .unwrap();
    assert!(found.is_some());
    assert_eq!(found.unwrap().device_id, cam.device_id);
}

#[tokio::test]
async fn get_camera_by_fingerprint_not_found() {
    let db = fresh_db().await;
    let found = db
        .get_camera_by_fingerprint(&CertFingerprint("nonexistent".to_string()))
        .await
        .unwrap();
    assert!(found.is_none());
}

#[tokio::test]
async fn get_camera_by_id() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-3")).await.unwrap();
    let found = db.get_camera(&cam.device_id).await.unwrap();
    assert!(found.is_some());
    assert_eq!(found.unwrap().cert_fingerprint.0, "fp-3");
}

#[tokio::test]
async fn get_camera_by_id_not_found() {
    let db = fresh_db().await;
    let found = db
        .get_camera(&DeviceId("nonexistent".to_string()))
        .await
        .unwrap();
    assert!(found.is_none());
}

#[tokio::test]
async fn list_cameras_empty() {
    let db = fresh_db().await;
    let cams = db.list_cameras(&solo_user()).await.unwrap();
    assert!(cams.is_empty());
}

#[tokio::test]
async fn list_cameras_returns_all() {
    let db = fresh_db().await;
    db.create_camera(&new_camera("fp-a")).await.unwrap();
    db.create_camera(&new_camera("fp-b")).await.unwrap();
    db.create_camera(&new_camera("fp-c")).await.unwrap();
    let cams = db.list_cameras(&solo_user()).await.unwrap();
    assert_eq!(cams.len(), 3);
}

#[tokio::test]
async fn update_camera_display_name() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-4")).await.unwrap();
    db.update_camera(
        &cam.device_id,
        &CameraUpdate {
            display_name: Some("Front Door".to_string()),
            notes: None,
        },
    )
    .await
    .unwrap();
    let found = db.get_camera(&cam.device_id).await.unwrap().unwrap();
    assert_eq!(found.display_name, "Front Door");
}

#[tokio::test]
async fn update_camera_notes() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-5")).await.unwrap();
    db.update_camera(
        &cam.device_id,
        &CameraUpdate {
            display_name: None,
            notes: Some("Mounted above garage".to_string()),
        },
    )
    .await
    .unwrap();
    let found = db.get_camera(&cam.device_id).await.unwrap().unwrap();
    assert_eq!(found.notes.unwrap(), "Mounted above garage");
}

#[tokio::test]
async fn update_camera_partial() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-6")).await.unwrap();
    db.update_camera(
        &cam.device_id,
        &CameraUpdate {
            display_name: None,
            notes: Some("note1".to_string()),
        },
    )
    .await
    .unwrap();
    db.update_camera(
        &cam.device_id,
        &CameraUpdate {
            display_name: Some("Renamed".to_string()),
            notes: None,
        },
    )
    .await
    .unwrap();
    let found = db.get_camera(&cam.device_id).await.unwrap().unwrap();
    assert_eq!(found.display_name, "Renamed");
    assert_eq!(found.notes.unwrap(), "note1");
}

#[tokio::test]
async fn delete_camera() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-7")).await.unwrap();
    db.delete_camera(&cam.device_id).await.unwrap();
    let found = db.get_camera(&cam.device_id).await.unwrap();
    assert!(found.is_none());
}

#[tokio::test]
async fn delete_camera_not_found() {
    let db = fresh_db().await;
    db.delete_camera(&DeviceId("nonexistent".to_string()))
        .await
        .unwrap();
}

#[tokio::test]
async fn update_last_seen() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-8")).await.unwrap();
    assert!(cam.last_seen_at.is_none());
    db.update_last_seen(&cam.device_id).await.unwrap();
    let found = db.get_camera(&cam.device_id).await.unwrap().unwrap();
    assert!(found.last_seen_at.is_some());
}

#[tokio::test]
async fn duplicate_fingerprint_rejected() {
    let db = fresh_db().await;
    db.create_camera(&new_camera("dup-fp")).await.unwrap();
    let result = db.create_camera(&new_camera("dup-fp")).await;
    assert!(result.is_err());
}

// --- Enrollment Tokens ---

#[tokio::test]
async fn create_and_claim_token() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-token-1")).await.unwrap();
    let future = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
        + 3600;

    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "tok-1".to_string(),
        user_id: solo_user(),
        expires_at: future,
    })
    .await
    .unwrap();

    let claimed = db
        .claim_enrollment_token("tok-1", &cam.device_id)
        .await
        .unwrap();
    assert!(claimed);
}

#[tokio::test]
async fn claim_token_twice_fails() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-token-2")).await.unwrap();
    let future = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
        + 3600;

    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "tok-2".to_string(),
        user_id: solo_user(),
        expires_at: future,
    })
    .await
    .unwrap();

    let first = db
        .claim_enrollment_token("tok-2", &cam.device_id)
        .await
        .unwrap();
    assert!(first);
    let second = db
        .claim_enrollment_token("tok-2", &cam.device_id)
        .await
        .unwrap();
    assert!(!second);
}

#[tokio::test]
async fn claim_expired_token_fails() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-token-3")).await.unwrap();

    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "tok-3".to_string(),
        user_id: solo_user(),
        expires_at: 1, // way in the past
    })
    .await
    .unwrap();

    let claimed = db
        .claim_enrollment_token("tok-3", &cam.device_id)
        .await
        .unwrap();
    assert!(!claimed);
}

#[tokio::test]
async fn claim_nonexistent_token() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-token-4")).await.unwrap();
    let claimed = db
        .claim_enrollment_token("nonexistent", &cam.device_id)
        .await
        .unwrap();
    assert!(!claimed);
}

#[tokio::test]
async fn cleanup_expired_tokens() {
    let db = fresh_db().await;
    let future = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
        + 3600;

    // 2 expired, 1 valid
    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "exp-1".to_string(),
        user_id: solo_user(),
        expires_at: 1,
    })
    .await
    .unwrap();
    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "exp-2".to_string(),
        user_id: solo_user(),
        expires_at: 1,
    })
    .await
    .unwrap();
    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "valid-1".to_string(),
        user_id: solo_user(),
        expires_at: future,
    })
    .await
    .unwrap();

    let deleted = db.cleanup_expired_tokens().await.unwrap();
    assert_eq!(deleted, 2);
}

#[tokio::test]
async fn cleanup_preserves_claimed() {
    let db = fresh_db().await;
    let cam = db.create_camera(&new_camera("fp-token-5")).await.unwrap();
    let future = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
        + 3600;

    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "claimed-exp".to_string(),
        user_id: solo_user(),
        expires_at: future,
    })
    .await
    .unwrap();
    db.claim_enrollment_token("claimed-exp", &cam.device_id)
        .await
        .unwrap();

    // Now make it "expired" by creating another expired unclaimed one
    db.create_enrollment_token(&NewEnrollmentToken {
        jti: "unclaimed-exp".to_string(),
        user_id: solo_user(),
        expires_at: 1,
    })
    .await
    .unwrap();

    let deleted = db.cleanup_expired_tokens().await.unwrap();
    assert_eq!(deleted, 1); // only the unclaimed expired one
}

// --- Sessions ---

fn new_session(id: &str) -> NewSession {
    NewSession {
        session_id: SessionId(id.to_string()),
        user_id: solo_user(),
        user_agent: None,
        ip_address: None,
    }
}

#[tokio::test]
async fn create_and_get_session() {
    let db = fresh_db().await;
    db.create_session(&new_session("sess-1")).await.unwrap();
    let found = db
        .get_session(&SessionId("sess-1".to_string()))
        .await
        .unwrap();
    assert!(found.is_some());
    let sess = found.unwrap();
    assert_eq!(sess.session_id.0, "sess-1");
    assert!(sess.expires_at > sess.created_at);
}

#[tokio::test]
async fn get_session_not_found() {
    let db = fresh_db().await;
    let found = db
        .get_session(&SessionId("nonexistent".to_string()))
        .await
        .unwrap();
    assert!(found.is_none());
}

#[tokio::test]
async fn delete_session() {
    let db = fresh_db().await;
    db.create_session(&new_session("sess-2")).await.unwrap();
    db.delete_session(&SessionId("sess-2".to_string()))
        .await
        .unwrap();
    let found = db
        .get_session(&SessionId("sess-2".to_string()))
        .await
        .unwrap();
    assert!(found.is_none());
}

#[tokio::test]
async fn extend_session() {
    let db = fresh_db().await;
    db.create_session(&new_session("sess-3")).await.unwrap();
    db.extend_session(&SessionId("sess-3".to_string()))
        .await
        .unwrap();
    let found = db
        .get_session(&SessionId("sess-3".to_string()))
        .await
        .unwrap()
        .unwrap();
    assert!(found.last_active_at.is_some());
}

#[tokio::test]
async fn cleanup_expired_sessions() {
    let db = fresh_db().await;
    // Create a normal session (expires in 30 days — won't be cleaned up)
    db.create_session(&new_session("valid-sess")).await.unwrap();

    // Manually insert expired sessions
    sqlx::query("INSERT INTO sessions (session_id, created_at, expires_at) VALUES ('exp-1', 1, 2)")
        .execute(db.pool_for_testing())
        .await
        .unwrap();
    sqlx::query("INSERT INTO sessions (session_id, created_at, expires_at) VALUES ('exp-2', 1, 3)")
        .execute(db.pool_for_testing())
        .await
        .unwrap();

    let deleted = db.cleanup_expired_sessions().await.unwrap();
    assert_eq!(deleted, 2);

    let valid = db
        .get_session(&SessionId("valid-sess".to_string()))
        .await
        .unwrap();
    assert!(valid.is_some());
}

#[tokio::test]
async fn session_user_agent() {
    let db = fresh_db().await;
    db.create_session(&NewSession {
        session_id: SessionId("sess-ua".to_string()),
        user_id: solo_user(),
        user_agent: Some("Mozilla/5.0".to_string()),
        ip_address: None,
    })
    .await
    .unwrap();
    // Session stored successfully (user_agent is write-only in current API)
    let found = db
        .get_session(&SessionId("sess-ua".to_string()))
        .await
        .unwrap();
    assert!(found.is_some());
}

// --- API Tokens ---

#[tokio::test]
async fn create_and_verify_api_token() {
    let db = fresh_db().await;
    db.initialize().await.unwrap();
    let secret = db.get_hmac_secret().await.unwrap();
    let (token_id, raw_token) = auth::generate_api_token();
    let hash = auth::hmac_token(&raw_token, &secret);

    db.create_api_token(&NewApiToken {
        token_id: token_id.clone(),
        user_id: solo_user(),
        token_hash: hash.clone(),
        label: "Test Token".to_string(),
        expires_at: None,
    })
    .await
    .unwrap();

    let found = db.verify_api_token(&hash).await.unwrap();
    assert!(found.is_some());
    assert_eq!(found.unwrap().token_id, token_id);
}

#[tokio::test]
async fn verify_wrong_token() {
    let db = fresh_db().await;
    let found = db.verify_api_token("nonexistent-hash").await.unwrap();
    assert!(found.is_none());
}

#[tokio::test]
async fn list_api_tokens_empty() {
    let db = fresh_db().await;
    let tokens = db.list_api_tokens(&solo_user()).await.unwrap();
    assert!(tokens.is_empty());
}

#[tokio::test]
async fn list_api_tokens() {
    let db = fresh_db().await;
    for i in 0..3 {
        db.create_api_token(&NewApiToken {
            token_id: TokenId(format!("tid-{}", i)),
            user_id: solo_user(),
            token_hash: format!("hash-{}", i),
            label: format!("Token {}", i),
            expires_at: None,
        })
        .await
        .unwrap();
    }
    let tokens = db.list_api_tokens(&solo_user()).await.unwrap();
    assert_eq!(tokens.len(), 3);
}

#[tokio::test]
async fn delete_api_token() {
    let db = fresh_db().await;
    let tid = TokenId("to-delete".to_string());
    db.create_api_token(&NewApiToken {
        token_id: tid.clone(),
        user_id: solo_user(),
        token_hash: "hash-del".to_string(),
        label: "Delete Me".to_string(),
        expires_at: None,
    })
    .await
    .unwrap();
    db.delete_api_token(&tid).await.unwrap();
    let tokens = db.list_api_tokens(&solo_user()).await.unwrap();
    assert!(tokens.is_empty());
}

#[tokio::test]
async fn api_token_label() {
    let db = fresh_db().await;
    db.create_api_token(&NewApiToken {
        token_id: TokenId("tid-label".to_string()),
        user_id: solo_user(),
        token_hash: "hash-label".to_string(),
        label: "Home Assistant".to_string(),
        expires_at: None,
    })
    .await
    .unwrap();
    let tokens = db.list_api_tokens(&solo_user()).await.unwrap();
    assert_eq!(tokens[0].label, "Home Assistant");
}

#[tokio::test]
async fn api_token_expiry() {
    let db = fresh_db().await;
    db.create_api_token(&NewApiToken {
        token_id: TokenId("tid-exp".to_string()),
        user_id: solo_user(),
        token_hash: "hash-exp".to_string(),
        label: "Expiring".to_string(),
        expires_at: Some(1700000000),
    })
    .await
    .unwrap();
    let tokens = db.list_api_tokens(&solo_user()).await.unwrap();
    assert_eq!(tokens[0].expires_at, Some(1700000000));
}

// --- Password Management ---

#[tokio::test]
async fn verify_initial_password() {
    let db = fresh_db().await;
    let pw = db.initialize().await.unwrap().unwrap();
    let verified = db.verify_password(&solo_user(), &pw).await.unwrap();
    assert!(verified);
}

#[tokio::test]
async fn set_password_and_verify() {
    let db = fresh_db().await;
    let old_pw = db.initialize().await.unwrap().unwrap();
    let new_hash = auth::hash_password("new-password").unwrap();
    db.set_password(&solo_user(), &new_hash).await.unwrap();

    assert!(!db.verify_password(&solo_user(), &old_pw).await.unwrap());
    assert!(db
        .verify_password(&solo_user(), "new-password")
        .await
        .unwrap());
}
