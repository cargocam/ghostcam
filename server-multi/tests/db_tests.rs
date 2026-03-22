/// Postgres integration tests. These require a running Postgres instance.
/// Run with: cargo test -p server-multi -- --ignored
/// Set DATABASE_URL env var, e.g.: DATABASE_URL=postgres://localhost/ghostcam_test
use ghostcam::types::{CertFingerprint, SessionId, TokenId, UserId};
use server_core::auth;
use server_core::db::{
    Database, NewApiToken, NewCameraRecord, NewEnrollmentToken, NewSession, UserUpdate,
};
use server_multi::db::PostgresDatabase;

async fn test_db() -> PostgresDatabase {
    let url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set for Postgres tests");
    let db = PostgresDatabase::connect(&url).await.unwrap();
    db.initialize().await.unwrap();
    db
}

async fn create_test_user(db: &PostgresDatabase) -> UserId {
    let email = format!("test-{}@example.com", uuid::Uuid::new_v4());
    let hash = auth::hash_password("password123").unwrap();
    db.create_user(&email, &hash, "Test User").await.unwrap()
}

// --- Initialization ---

#[tokio::test]
#[ignore]
async fn connect_and_migrate() {
    let _db = test_db().await;
}

#[tokio::test]
#[ignore]
async fn migrate_is_idempotent() {
    let db = test_db().await;
    db.migrate().await.unwrap();
    db.migrate().await.unwrap();
}

#[tokio::test]
#[ignore]
async fn initialize_creates_hmac_secret() {
    let db = test_db().await;
    let secret = db.get_hmac_secret().await.unwrap();
    assert_eq!(secret.len(), 32);
}

#[tokio::test]
#[ignore]
async fn health_check_succeeds() {
    let db = test_db().await;
    db.health_check().await.unwrap();
}

// --- User Management ---

#[tokio::test]
#[ignore]
async fn create_user() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    assert!(uuid::Uuid::parse_str(&user_id.0).is_ok());
}

#[tokio::test]
#[ignore]
async fn create_user_duplicate_email() {
    let db = test_db().await;
    let email = format!("dup-{}@example.com", uuid::Uuid::new_v4());
    let hash = auth::hash_password("pw").unwrap();
    db.create_user(&email, &hash, "User A").await.unwrap();
    let result = db.create_user(&email, &hash, "User B").await;
    assert!(result.is_err());
}

#[tokio::test]
#[ignore]
async fn get_user_by_email() {
    let db = test_db().await;
    let email = format!("find-{}@example.com", uuid::Uuid::new_v4());
    let hash = auth::hash_password("pw").unwrap();
    db.create_user(&email, &hash, "Findable").await.unwrap();
    let found = db.get_user_by_email(&email).await.unwrap();
    assert!(found.is_some());
    assert_eq!(found.unwrap().display_name, "Findable");
}

#[tokio::test]
#[ignore]
async fn get_user_by_email_case_insensitive() {
    let db = test_db().await;
    let unique = uuid::Uuid::new_v4();
    let email = format!("Case-{}@Example.COM", unique);
    let hash = auth::hash_password("pw").unwrap();
    db.create_user(&email, &hash, "CaseUser").await.unwrap();
    let found = db
        .get_user_by_email(&format!("case-{}@example.com", unique))
        .await
        .unwrap();
    assert!(found.is_some());
}

#[tokio::test]
#[ignore]
async fn get_user_by_id() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    let found = db.get_user(&user_id).await.unwrap();
    assert!(found.is_some());
}

#[tokio::test]
#[ignore]
async fn update_user_display_name() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    db.update_user(
        &user_id,
        &UserUpdate {
            display_name: Some("Updated Name".to_string()),
            ..Default::default()
        },
    )
    .await
    .unwrap();
    let found = db.get_user(&user_id).await.unwrap().unwrap();
    assert_eq!(found.display_name, "Updated Name");
}

#[tokio::test]
#[ignore]
async fn update_user_email() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    let new_email = format!("new-{}@example.com", uuid::Uuid::new_v4());
    db.update_user(
        &user_id,
        &UserUpdate {
            email: Some(new_email.clone()),
            ..Default::default()
        },
    )
    .await
    .unwrap();
    let found = db.get_user(&user_id).await.unwrap().unwrap();
    assert_eq!(found.email, new_email);
}

// --- Camera CRUD (scoped to user) ---

#[tokio::test]
#[ignore]
async fn create_camera_for_user() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    let cam = db
        .create_camera(&NewCameraRecord {
            user_id: user_id.clone(),
            cert_fingerprint: CertFingerprint(format!("fp-{}", uuid::Uuid::new_v4())),
            display_name: "Test Cam".to_string(),
        })
        .await
        .unwrap();
    assert!(uuid::Uuid::parse_str(&cam.device_id.0).is_ok());
    assert_eq!(cam.user_id, user_id);
}

#[tokio::test]
#[ignore]
async fn list_cameras_scoped_to_user() {
    let db = test_db().await;
    let user_a = create_test_user(&db).await;
    let user_b = create_test_user(&db).await;

    for i in 0..2 {
        db.create_camera(&NewCameraRecord {
            user_id: user_a.clone(),
            cert_fingerprint: CertFingerprint(format!("fp-a-{}", i)),
            display_name: "Cam A".to_string(),
        })
        .await
        .unwrap();
        db.create_camera(&NewCameraRecord {
            user_id: user_b.clone(),
            cert_fingerprint: CertFingerprint(format!("fp-b-{}", i)),
            display_name: "Cam B".to_string(),
        })
        .await
        .unwrap();
    }

    let cams_a = db.list_cameras(&user_a).await.unwrap();
    assert_eq!(cams_a.len(), 2);
    assert!(cams_a.iter().all(|c| c.user_id == user_a));
}

#[tokio::test]
#[ignore]
async fn get_camera_by_fingerprint() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    let fp = format!("fp-find-{}", uuid::Uuid::new_v4());
    db.create_camera(&NewCameraRecord {
        user_id: user_id.clone(),
        cert_fingerprint: CertFingerprint(fp.clone()),
        display_name: "Findable".to_string(),
    })
    .await
    .unwrap();
    let found = db
        .get_camera_by_fingerprint(&CertFingerprint(fp))
        .await
        .unwrap();
    assert!(found.is_some());
    assert_eq!(found.unwrap().user_id, user_id);
}

// --- Enrollment Tokens (scoped to user) ---

#[tokio::test]
#[ignore]
async fn create_token_for_user() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    db.create_enrollment_token(&NewEnrollmentToken {
        jti: format!("jti-{}", uuid::Uuid::new_v4()),
        user_id,
        expires_at: 9999999999,
    })
    .await
    .unwrap();
}

#[tokio::test]
#[ignore]
async fn claim_token() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    let jti = format!("jti-{}", uuid::Uuid::new_v4());
    db.create_enrollment_token(&NewEnrollmentToken {
        jti: jti.clone(),
        user_id: user_id.clone(),
        expires_at: 9999999999,
    })
    .await
    .unwrap();
    let cam = db
        .create_camera(&NewCameraRecord {
            user_id,
            cert_fingerprint: CertFingerprint(format!("fp-claim-{}", uuid::Uuid::new_v4())),
            display_name: "Claimed".to_string(),
        })
        .await
        .unwrap();
    let claimed = db
        .claim_enrollment_token(&jti, &cam.device_id)
        .await
        .unwrap();
    assert!(claimed);
}

#[tokio::test]
#[ignore]
async fn claim_token_twice_fails() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    let jti = format!("jti-{}", uuid::Uuid::new_v4());
    db.create_enrollment_token(&NewEnrollmentToken {
        jti: jti.clone(),
        user_id: user_id.clone(),
        expires_at: 9999999999,
    })
    .await
    .unwrap();
    let cam = db
        .create_camera(&NewCameraRecord {
            user_id,
            cert_fingerprint: CertFingerprint(format!("fp-claim2-{}", uuid::Uuid::new_v4())),
            display_name: "Claimed".to_string(),
        })
        .await
        .unwrap();
    assert!(db
        .claim_enrollment_token(&jti, &cam.device_id)
        .await
        .unwrap());
    assert!(!db
        .claim_enrollment_token(&jti, &cam.device_id)
        .await
        .unwrap());
}

// --- Sessions (scoped to user) ---

#[tokio::test]
#[ignore]
async fn create_session_for_user() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    db.create_session(&NewSession {
        session_id: SessionId(uuid::Uuid::new_v4().to_string()),
        user_id: user_id.clone(),
        user_agent: Some("Test/1.0".to_string()),
        ip_address: None,
    })
    .await
    .unwrap();
}

#[tokio::test]
#[ignore]
async fn session_stores_ip_address() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    let sid = SessionId(uuid::Uuid::new_v4().to_string());
    db.create_session(&NewSession {
        session_id: sid.clone(),
        user_id,
        user_agent: None,
        ip_address: Some("192.168.1.1".to_string()),
    })
    .await
    .unwrap();
    let found = db.get_session(&sid).await.unwrap();
    assert!(found.is_some());
}

// --- API Tokens (scoped to user) ---

#[tokio::test]
#[ignore]
async fn create_api_token_for_user() {
    let db = test_db().await;
    let user_id = create_test_user(&db).await;
    db.create_api_token(&NewApiToken {
        token_id: TokenId(uuid::Uuid::new_v4().to_string()),
        user_id,
        token_hash: format!("hash-{}", uuid::Uuid::new_v4()),
        label: "Test Token".to_string(),
        expires_at: None,
    })
    .await
    .unwrap();
}

#[tokio::test]
#[ignore]
async fn list_tokens_scoped_to_user() {
    let db = test_db().await;
    let user_a = create_test_user(&db).await;
    let user_b = create_test_user(&db).await;

    for i in 0..2 {
        db.create_api_token(&NewApiToken {
            token_id: TokenId(uuid::Uuid::new_v4().to_string()),
            user_id: user_a.clone(),
            token_hash: format!("hash-a-{}", i),
            label: "A".to_string(),
            expires_at: None,
        })
        .await
        .unwrap();
        db.create_api_token(&NewApiToken {
            token_id: TokenId(uuid::Uuid::new_v4().to_string()),
            user_id: user_b.clone(),
            token_hash: format!("hash-b-{}", i),
            label: "B".to_string(),
            expires_at: None,
        })
        .await
        .unwrap();
    }

    let tokens = db.list_api_tokens(&user_a).await.unwrap();
    assert_eq!(tokens.len(), 2);
}

// --- Password Management ---

#[tokio::test]
#[ignore]
async fn verify_password_by_user_id() {
    let db = test_db().await;
    let hash = auth::hash_password("mypassword").unwrap();
    let email = format!("pw-{}@example.com", uuid::Uuid::new_v4());
    let user_id = db.create_user(&email, &hash, "PW User").await.unwrap();
    assert!(db.verify_password(&user_id, "mypassword").await.unwrap());
    assert!(!db.verify_password(&user_id, "wrongpassword").await.unwrap());
}

#[tokio::test]
#[ignore]
async fn set_password() {
    let db = test_db().await;
    let hash = auth::hash_password("old").unwrap();
    let email = format!("setpw-{}@example.com", uuid::Uuid::new_v4());
    let user_id = db.create_user(&email, &hash, "SetPW").await.unwrap();
    let new_hash = auth::hash_password("new").unwrap();
    db.set_password(&user_id, &new_hash).await.unwrap();
    assert!(!db.verify_password(&user_id, "old").await.unwrap());
    assert!(db.verify_password(&user_id, "new").await.unwrap());
}
