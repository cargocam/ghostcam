use crate::auth;
use crate::db_trait::{
    ApiTokenRecord, AuditLogRecord, CameraRecord, CameraUpdate, Database, NewApiToken,
    NewCameraRecord, NewEnrollmentToken, NewSession, SessionRecord, UserRecord, UserUpdate,
};
use anyhow::{Context, Result};
use async_trait::async_trait;
use ghostcam::types::{CertFingerprint, DeviceId, SessionId, TokenId, UserId};
use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Row};
use std::time::{SystemTime, UNIX_EPOCH};

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs()
}

pub struct PostgresDatabase {
    pool: PgPool,
}

impl PostgresDatabase {
    pub async fn connect(url: &str) -> Result<Self> {
        let max_conns: u32 = std::env::var("GHOSTCAM_DB_POOL_SIZE")
            .ok()
            .and_then(|v| match v.parse::<u32>() {
                Ok(n) if n > 0 => Some(n),
                Ok(_) => {
                    tracing::warn!("GHOSTCAM_DB_POOL_SIZE=0 is invalid, using default 20");
                    None
                }
                Err(_) => {
                    tracing::warn!(
                        "GHOSTCAM_DB_POOL_SIZE={v:?} is not a valid u32, using default 20"
                    );
                    None
                }
            })
            .unwrap_or(20);
        let max_conns = if max_conns > 200 {
            tracing::warn!("GHOSTCAM_DB_POOL_SIZE={max_conns} exceeds cap, clamping to 200");
            200
        } else {
            max_conns
        };
        let pool = PgPoolOptions::new()
            .max_connections(max_conns)
            .connect(url)
            .await
            .context("failed to connect to PostgreSQL")?;

        let db = Self { pool };
        db.migrate().await?;
        Ok(db)
    }

    /// First-run initialization. Returns the initial password if one was generated.
    /// If `preset_password` is provided it will be used instead of a random one.
    pub async fn initialize(
        &self,
        preset_password: Option<&str>,
        admin_email: &str,
    ) -> Result<Option<String>> {
        let has_users: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM users)")
            .fetch_one(&self.pool)
            .await?;

        let initial_password = if !has_users {
            let password = preset_password
                .map(str::to_owned)
                .unwrap_or_else(auth::generate_random_password);
            let hash = auth::hash_password(&password)?;
            let user_id = uuid::Uuid::new_v4().to_string();
            let now = now_unix() as i64;

            sqlx::query(
                "INSERT INTO users (user_id, email, password_hash, display_name, created_at, password_changed_at) \
                 VALUES ($1, $2, $3, 'Admin', $4, $4)"
            )
            .bind(&user_id)
            .bind(admin_email)
            .bind(&hash)
            .bind(now)
            .execute(&self.pool)
            .await?;

            Some(password)
        } else {
            None
        };

        // Ensure HMAC secret exists
        let secret_exists: bool =
            sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM config WHERE key = 'hmac_secret')")
                .fetch_one(&self.pool)
                .await?;

        if !secret_exists {
            let secret = auth::generate_hmac_secret();
            sqlx::query("INSERT INTO config (key, value) VALUES ('hmac_secret', $1)")
                .bind(&secret)
                .execute(&self.pool)
                .await?;
        }

        Ok(initial_password)
    }
}

#[async_trait]
impl Database for PostgresDatabase {
    // --- Camera operations ---

    async fn get_camera_by_fingerprint(
        &self,
        fingerprint: &CertFingerprint,
    ) -> Result<Option<CameraRecord>> {
        let row = sqlx::query(
            "SELECT device_id, user_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras WHERE cert_fingerprint = $1",
        )
        .bind(&fingerprint.0)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| CameraRecord {
            device_id: DeviceId(r.get("device_id")),
            user_id: UserId(r.get("user_id")),
            cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
            display_name: r.get("display_name"),
            enrolled_at: r.get::<i64, _>("enrolled_at") as u64,
            last_seen_at: r.get::<Option<i64>, _>("last_seen_at").map(|v| v as u64),
            notes: r.get("notes"),
        }))
    }

    async fn get_camera(&self, device_id: &DeviceId) -> Result<Option<CameraRecord>> {
        let row = sqlx::query(
            "SELECT device_id, user_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras WHERE device_id = $1",
        )
        .bind(&device_id.0)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| CameraRecord {
            device_id: DeviceId(r.get("device_id")),
            user_id: UserId(r.get("user_id")),
            cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
            display_name: r.get("display_name"),
            enrolled_at: r.get::<i64, _>("enrolled_at") as u64,
            last_seen_at: r.get::<Option<i64>, _>("last_seen_at").map(|v| v as u64),
            notes: r.get("notes"),
        }))
    }

    async fn list_cameras(&self, user_id: &UserId) -> Result<Vec<CameraRecord>> {
        let rows = sqlx::query(
            "SELECT device_id, user_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras WHERE user_id = $1 ORDER BY enrolled_at",
        )
        .bind(&user_id.0)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .into_iter()
            .map(|r| CameraRecord {
                device_id: DeviceId(r.get("device_id")),
                user_id: UserId(r.get("user_id")),
                cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
                display_name: r.get("display_name"),
                enrolled_at: r.get::<i64, _>("enrolled_at") as u64,
                last_seen_at: r.get::<Option<i64>, _>("last_seen_at").map(|v| v as u64),
                notes: r.get("notes"),
            })
            .collect())
    }

    async fn create_camera(&self, record: &NewCameraRecord) -> Result<CameraRecord> {
        let device_id = uuid::Uuid::new_v4().to_string();
        let now = now_unix() as i64;

        sqlx::query(
            "INSERT INTO cameras (device_id, user_id, cert_fingerprint, display_name, enrolled_at) VALUES ($1, $2, $3, $4, $5)",
        )
        .bind(&device_id)
        .bind(&record.user_id.0)
        .bind(&record.cert_fingerprint.0)
        .bind(&record.display_name)
        .bind(now)
        .execute(&self.pool)
        .await?;

        Ok(CameraRecord {
            device_id: DeviceId(device_id),
            user_id: record.user_id.clone(),
            cert_fingerprint: record.cert_fingerprint.clone(),
            display_name: record.display_name.clone(),
            enrolled_at: now as u64,
            last_seen_at: None,
            notes: None,
        })
    }

    async fn update_camera(&self, device_id: &DeviceId, update: &CameraUpdate) -> Result<()> {
        if update.display_name.is_none() && update.notes.is_none() {
            return Ok(());
        }
        sqlx::query(
            "UPDATE cameras SET \
             display_name = COALESCE($1, display_name), \
             notes = COALESCE($2, notes) \
             WHERE device_id = $3",
        )
        .bind(update.display_name.as_deref())
        .bind(update.notes.as_deref())
        .bind(&device_id.0)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn delete_camera(&self, device_id: &DeviceId) -> Result<()> {
        sqlx::query("DELETE FROM cameras WHERE device_id = $1")
            .bind(&device_id.0)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn update_last_seen(&self, device_id: &DeviceId) -> Result<()> {
        let now = now_unix() as i64;
        sqlx::query("UPDATE cameras SET last_seen_at = $1 WHERE device_id = $2")
            .bind(now)
            .bind(&device_id.0)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    // --- Enrollment tokens ---

    async fn create_enrollment_token(&self, token: &NewEnrollmentToken) -> Result<()> {
        sqlx::query("INSERT INTO enrollment_tokens (jti, user_id, expires_at) VALUES ($1, $2, $3)")
            .bind(&token.jti)
            .bind(&token.user_id.0)
            .bind(token.expires_at as i64)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn claim_enrollment_token(&self, jti: &str, device_id: &DeviceId) -> Result<bool> {
        let now = now_unix() as i64;
        let result = sqlx::query(
            "UPDATE enrollment_tokens SET claimed_by = $1, claimed_at = $2 WHERE jti = $3 AND claimed_by IS NULL AND expires_at > $2",
        )
        .bind(&device_id.0)
        .bind(now)
        .bind(jti)
        .execute(&self.pool)
        .await?;

        Ok(result.rows_affected() > 0)
    }

    async fn cleanup_expired_tokens(&self) -> Result<u64> {
        let now = now_unix() as i64;
        let result = sqlx::query(
            "DELETE FROM enrollment_tokens WHERE expires_at < $1 AND claimed_by IS NULL",
        )
        .bind(now)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected())
    }

    async fn get_enrollment_token_user_id(&self, jti: &str) -> Result<Option<UserId>> {
        let now = now_unix() as i64;
        let row = sqlx::query(
            "SELECT user_id FROM enrollment_tokens WHERE jti = $1 AND claimed_by IS NULL AND expires_at > $2"
        )
        .bind(jti)
        .bind(now)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| UserId(r.get("user_id"))))
    }

    // --- Sessions ---

    async fn create_session(&self, session: &NewSession) -> Result<()> {
        let now = now_unix() as i64;
        let expires_at = now + 86400 * 30; // 30 days

        sqlx::query(
            "INSERT INTO sessions (session_id, user_id, created_at, expires_at, user_agent, ip_address) VALUES ($1, $2, $3, $4, $5, $6)",
        )
        .bind(&session.session_id.0)
        .bind(&session.user_id.0)
        .bind(now)
        .bind(expires_at)
        .bind(&session.user_agent)
        .bind(&session.ip_address)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn get_session(&self, session_id: &SessionId) -> Result<Option<SessionRecord>> {
        let row = sqlx::query(
            "SELECT session_id, user_id, created_at, expires_at, last_active_at FROM sessions WHERE session_id = $1",
        )
        .bind(&session_id.0)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| SessionRecord {
            session_id: SessionId(r.get("session_id")),
            user_id: UserId(r.get("user_id")),
            created_at: r.get::<i64, _>("created_at") as u64,
            expires_at: r.get::<i64, _>("expires_at") as u64,
            last_active_at: r.get::<Option<i64>, _>("last_active_at").map(|v| v as u64),
        }))
    }

    async fn delete_session(&self, session_id: &SessionId) -> Result<()> {
        sqlx::query("DELETE FROM sessions WHERE session_id = $1")
            .bind(&session_id.0)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn extend_session(&self, session_id: &SessionId) -> Result<()> {
        let now = now_unix() as i64;
        sqlx::query("UPDATE sessions SET last_active_at = $1 WHERE session_id = $2")
            .bind(now)
            .bind(&session_id.0)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn cleanup_expired_sessions(&self) -> Result<u64> {
        let now = now_unix() as i64;
        let result = sqlx::query("DELETE FROM sessions WHERE expires_at < $1")
            .bind(now)
            .execute(&self.pool)
            .await?;
        Ok(result.rows_affected())
    }

    // --- API tokens ---

    async fn create_api_token(&self, token: &NewApiToken) -> Result<()> {
        let now = now_unix() as i64;
        sqlx::query(
            "INSERT INTO api_tokens (token_id, user_id, token_hash, label, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)",
        )
        .bind(&token.token_id.0)
        .bind(&token.user_id.0)
        .bind(&token.token_hash)
        .bind(&token.label)
        .bind(now)
        .bind(token.expires_at.map(|v| v as i64))
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn list_api_tokens(&self, user_id: &UserId) -> Result<Vec<ApiTokenRecord>> {
        let rows = sqlx::query(
            "SELECT token_id, user_id, label, created_at, expires_at, last_used_at FROM api_tokens WHERE user_id = $1 ORDER BY created_at",
        )
        .bind(&user_id.0)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .into_iter()
            .map(|r| ApiTokenRecord {
                token_id: TokenId(r.get("token_id")),
                user_id: UserId(r.get("user_id")),
                label: r.get("label"),
                created_at: r.get::<i64, _>("created_at") as u64,
                expires_at: r.get::<Option<i64>, _>("expires_at").map(|v| v as u64),
                last_used_at: r.get::<Option<i64>, _>("last_used_at").map(|v| v as u64),
            })
            .collect())
    }

    async fn verify_api_token(&self, token_hash: &str) -> Result<Option<ApiTokenRecord>> {
        let row = sqlx::query(
            "SELECT token_id, user_id, label, created_at, expires_at, last_used_at FROM api_tokens WHERE token_hash = $1",
        )
        .bind(token_hash)
        .fetch_optional(&self.pool)
        .await?;

        if let Some(r) = row {
            let now = now_unix() as i64;
            let token_id: String = r.get("token_id");
            sqlx::query("UPDATE api_tokens SET last_used_at = $1 WHERE token_id = $2")
                .bind(now)
                .bind(&token_id)
                .execute(&self.pool)
                .await?;

            Ok(Some(ApiTokenRecord {
                token_id: TokenId(token_id),
                user_id: UserId(r.get("user_id")),
                label: r.get("label"),
                created_at: r.get::<i64, _>("created_at") as u64,
                expires_at: r.get::<Option<i64>, _>("expires_at").map(|v| v as u64),
                last_used_at: Some(now as u64),
            }))
        } else {
            Ok(None)
        }
    }

    async fn delete_api_token(&self, token_id: &TokenId) -> Result<()> {
        sqlx::query("DELETE FROM api_tokens WHERE token_id = $1")
            .bind(&token_id.0)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    // --- Auth ---

    async fn verify_password(&self, user_id: &UserId, password: &str) -> Result<bool> {
        let row = sqlx::query("SELECT password_hash FROM users WHERE user_id = $1")
            .bind(&user_id.0)
            .fetch_optional(&self.pool)
            .await?;

        match row {
            Some(r) => {
                let hash: String = r.get("password_hash");
                auth::verify_password(password, &hash)
            }
            None => Ok(false),
        }
    }

    async fn set_password(&self, user_id: &UserId, password_hash: &str) -> Result<()> {
        let now = now_unix() as i64;
        sqlx::query(
            "UPDATE users SET password_hash = $1, password_changed_at = $2 WHERE user_id = $3",
        )
        .bind(password_hash)
        .bind(now)
        .bind(&user_id.0)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    // --- Server config ---

    async fn get_hmac_secret(&self) -> Result<Vec<u8>> {
        let row = sqlx::query("SELECT value FROM config WHERE key = 'hmac_secret'")
            .fetch_one(&self.pool)
            .await
            .context("HMAC secret not found in config table")?;

        Ok(row.get("value"))
    }

    // --- Lifecycle ---

    async fn migrate(&self) -> Result<()> {
        sqlx::raw_sql(include_str!("../migrations/001_initial.sql"))
            .execute(&self.pool)
            .await
            .context("failed to run migration 001")?;
        sqlx::raw_sql(include_str!("../migrations/002_multi_user.sql"))
            .execute(&self.pool)
            .await
            .context("failed to run migration 002")?;
        sqlx::raw_sql(include_str!("../migrations/003_audit_log.sql"))
            .execute(&self.pool)
            .await
            .context("failed to run migration 003")?;
        Ok(())
    }

    async fn health_check(&self) -> Result<()> {
        sqlx::query("SELECT 1")
            .fetch_one(&self.pool)
            .await
            .context("database health check failed")?;
        Ok(())
    }

    // --- User management ---

    async fn create_user(
        &self,
        email: &str,
        password_hash: &str,
        display_name: &str,
    ) -> Result<UserId> {
        let user_id = uuid::Uuid::new_v4().to_string();
        let now = now_unix() as i64;
        sqlx::query(
            "INSERT INTO users (user_id, email, password_hash, display_name, created_at, password_changed_at) \
             VALUES ($1, $2, $3, $4, $5, $5)"
        )
        .bind(&user_id)
        .bind(email)
        .bind(password_hash)
        .bind(display_name)
        .bind(now)
        .execute(&self.pool)
        .await?;
        Ok(UserId(user_id))
    }

    async fn get_user_by_email(&self, email: &str) -> Result<Option<UserRecord>> {
        let row = sqlx::query(
            "SELECT user_id, email, display_name, created_at, verified_at, disabled_at FROM users WHERE email = $1"
        )
        .bind(email)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| UserRecord {
            user_id: UserId(r.get("user_id")),
            email: r.get("email"),
            display_name: r.get("display_name"),
            created_at: r.get::<i64, _>("created_at") as u64,
            verified_at: r.get::<Option<i64>, _>("verified_at").map(|v| v as u64),
            disabled_at: r.get::<Option<i64>, _>("disabled_at").map(|v| v as u64),
        }))
    }

    async fn get_user(&self, user_id: &UserId) -> Result<Option<UserRecord>> {
        let row = sqlx::query(
            "SELECT user_id, email, display_name, created_at, verified_at, disabled_at FROM users WHERE user_id = $1"
        )
        .bind(&user_id.0)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| UserRecord {
            user_id: UserId(r.get("user_id")),
            email: r.get("email"),
            display_name: r.get("display_name"),
            created_at: r.get::<i64, _>("created_at") as u64,
            verified_at: r.get::<Option<i64>, _>("verified_at").map(|v| v as u64),
            disabled_at: r.get::<Option<i64>, _>("disabled_at").map(|v| v as u64),
        }))
    }

    // --- Audit log ---

    async fn insert_audit_entry(
        &self,
        timestamp: &str,
        event_type: &str,
        event_data: &serde_json::Value,
        hmac: &str,
    ) -> Result<()> {
        sqlx::query(
            "INSERT INTO audit_log (timestamp, event_type, event_data, hmac) VALUES ($1::timestamptz, $2, $3, $4)",
        )
        .bind(timestamp)
        .bind(event_type)
        .bind(event_data)
        .bind(hmac)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn query_audit_log(
        &self,
        event_type: Option<&str>,
        since: Option<&str>,
        until: Option<&str>,
        limit: i64,
        offset: i64,
    ) -> Result<Vec<AuditLogRecord>> {
        let rows = sqlx::query(
            "SELECT id, timestamp::text, event_type, event_data, hmac FROM audit_log \
             WHERE ($1::text IS NULL OR event_type = $1) \
             AND ($2::timestamptz IS NULL OR timestamp >= $2::timestamptz) \
             AND ($3::timestamptz IS NULL OR timestamp <= $3::timestamptz) \
             ORDER BY timestamp DESC \
             LIMIT $4 OFFSET $5",
        )
        .bind(event_type)
        .bind(since)
        .bind(until)
        .bind(limit)
        .bind(offset)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .into_iter()
            .map(|r| AuditLogRecord {
                id: r.get("id"),
                timestamp: r.get("timestamp"),
                event_type: r.get("event_type"),
                event_data: r.get("event_data"),
                hmac: r.get("hmac"),
            })
            .collect())
    }

    async fn count_audit_log(
        &self,
        event_type: Option<&str>,
        since: Option<&str>,
        until: Option<&str>,
    ) -> Result<i64> {
        let count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM audit_log \
             WHERE ($1::text IS NULL OR event_type = $1) \
             AND ($2::timestamptz IS NULL OR timestamp >= $2::timestamptz) \
             AND ($3::timestamptz IS NULL OR timestamp <= $3::timestamptz)",
        )
        .bind(event_type)
        .bind(since)
        .bind(until)
        .fetch_one(&self.pool)
        .await?;
        Ok(count)
    }

    async fn update_user(&self, user_id: &UserId, update: &UserUpdate) -> Result<()> {
        if update.email.is_none() && update.display_name.is_none() && update.password_hash.is_none()
        {
            return Ok(());
        }
        let now = now_unix() as i64;
        sqlx::query(
            "UPDATE users SET \
             email = COALESCE($1, email), \
             display_name = COALESCE($2, display_name), \
             password_hash = COALESCE($3, password_hash), \
             password_changed_at = CASE WHEN $3 IS NOT NULL THEN $4 ELSE password_changed_at END \
             WHERE user_id = $5",
        )
        .bind(update.email.as_deref())
        .bind(update.display_name.as_deref())
        .bind(update.password_hash.as_deref())
        .bind(now)
        .bind(&user_id.0)
        .execute(&self.pool)
        .await?;
        Ok(())
    }
}
