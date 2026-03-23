use anyhow::{Context, Result};
use async_trait::async_trait;
use ghostcam::types::{CertFingerprint, DeviceId, SessionId, TokenId, UserId};
use crate::auth;
use crate::db_trait::{
    ApiTokenRecord, CameraRecord, CameraUpdate, Database, NewApiToken, NewCameraRecord,
    NewEnrollmentToken, NewSession, SessionRecord, UserRecord, UserUpdate, SOLO_USER_ID,
};
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
        let pool = PgPoolOptions::new()
            .max_connections(5)
            .connect(url)
            .await
            .context("failed to connect to PostgreSQL")?;

        let db = Self { pool };
        db.migrate().await?;
        Ok(db)
    }

    /// First-run initialization. Returns the initial password if one was generated.
    /// If `preset_password` is provided it will be used instead of a random one.
    pub async fn initialize(&self, preset_password: Option<&str>) -> Result<Option<String>> {
        // Check if owner exists
        let owner_exists: bool =
            sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM owner WHERE id = 1)")
                .fetch_one(&self.pool)
                .await?;

        let initial_password = if !owner_exists {
            let password = preset_password
                .map(str::to_owned)
                .unwrap_or_else(auth::generate_random_password);
            let hash = auth::hash_password(&password)?;
            let now = now_unix() as i64;

            sqlx::query(
                "INSERT INTO owner (id, password_hash, display_name, password_changed_at) VALUES (1, $1, 'Operator', $2)",
            )
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
            "SELECT device_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras WHERE cert_fingerprint = $1",
        )
        .bind(&fingerprint.0)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| CameraRecord {
            device_id: DeviceId(r.get("device_id")),
            user_id: UserId(SOLO_USER_ID.to_string()),
            cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
            display_name: r.get("display_name"),
            enrolled_at: r.get::<i64, _>("enrolled_at") as u64,
            last_seen_at: r.get::<Option<i64>, _>("last_seen_at").map(|v| v as u64),
            notes: r.get("notes"),
        }))
    }

    async fn get_camera(&self, device_id: &DeviceId) -> Result<Option<CameraRecord>> {
        let row = sqlx::query(
            "SELECT device_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras WHERE device_id = $1",
        )
        .bind(&device_id.0)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| CameraRecord {
            device_id: DeviceId(r.get("device_id")),
            user_id: UserId(SOLO_USER_ID.to_string()),
            cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
            display_name: r.get("display_name"),
            enrolled_at: r.get::<i64, _>("enrolled_at") as u64,
            last_seen_at: r.get::<Option<i64>, _>("last_seen_at").map(|v| v as u64),
            notes: r.get("notes"),
        }))
    }

    async fn list_cameras(&self, _user_id: &UserId) -> Result<Vec<CameraRecord>> {
        let rows = sqlx::query(
            "SELECT device_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras ORDER BY enrolled_at",
        )
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .into_iter()
            .map(|r| CameraRecord {
                device_id: DeviceId(r.get("device_id")),
                user_id: UserId(SOLO_USER_ID.to_string()),
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
            "INSERT INTO cameras (device_id, cert_fingerprint, display_name, enrolled_at) VALUES ($1, $2, $3, $4)",
        )
        .bind(&device_id)
        .bind(&record.cert_fingerprint.0)
        .bind(&record.display_name)
        .bind(now)
        .execute(&self.pool)
        .await?;

        Ok(CameraRecord {
            device_id: DeviceId(device_id),
            user_id: UserId(SOLO_USER_ID.to_string()),
            cert_fingerprint: record.cert_fingerprint.clone(),
            display_name: record.display_name.clone(),
            enrolled_at: now as u64,
            last_seen_at: None,
            notes: None,
        })
    }

    async fn update_camera(&self, device_id: &DeviceId, update: &CameraUpdate) -> Result<()> {
        if let Some(ref name) = update.display_name {
            sqlx::query("UPDATE cameras SET display_name = $1 WHERE device_id = $2")
                .bind(name)
                .bind(&device_id.0)
                .execute(&self.pool)
                .await?;
        }
        if let Some(ref notes) = update.notes {
            sqlx::query("UPDATE cameras SET notes = $1 WHERE device_id = $2")
                .bind(notes)
                .bind(&device_id.0)
                .execute(&self.pool)
                .await?;
        }
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
        sqlx::query("INSERT INTO enrollment_tokens (jti, expires_at) VALUES ($1, $2)")
            .bind(&token.jti)
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

    // --- Sessions ---

    async fn create_session(&self, session: &NewSession) -> Result<()> {
        let now = now_unix() as i64;
        let expires_at = now + 86400 * 30; // 30 days

        sqlx::query(
            "INSERT INTO sessions (session_id, created_at, expires_at, user_agent, ip_address) VALUES ($1, $2, $3, $4, $5)",
        )
        .bind(&session.session_id.0)
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
            "SELECT session_id, created_at, expires_at, last_active_at FROM sessions WHERE session_id = $1",
        )
        .bind(&session_id.0)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| SessionRecord {
            session_id: SessionId(r.get("session_id")),
            user_id: UserId(SOLO_USER_ID.to_string()),
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
            "INSERT INTO api_tokens (token_id, token_hash, label, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)",
        )
        .bind(&token.token_id.0)
        .bind(&token.token_hash)
        .bind(&token.label)
        .bind(now)
        .bind(token.expires_at.map(|v| v as i64))
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn list_api_tokens(&self, _user_id: &UserId) -> Result<Vec<ApiTokenRecord>> {
        let rows = sqlx::query(
            "SELECT token_id, label, created_at, expires_at, last_used_at FROM api_tokens ORDER BY created_at",
        )
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .into_iter()
            .map(|r| ApiTokenRecord {
                token_id: TokenId(r.get("token_id")),
                user_id: UserId(SOLO_USER_ID.to_string()),
                label: r.get("label"),
                created_at: r.get::<i64, _>("created_at") as u64,
                expires_at: r.get::<Option<i64>, _>("expires_at").map(|v| v as u64),
                last_used_at: r.get::<Option<i64>, _>("last_used_at").map(|v| v as u64),
            })
            .collect())
    }

    async fn verify_api_token(&self, token_hash: &str) -> Result<Option<ApiTokenRecord>> {
        let row = sqlx::query(
            "SELECT token_id, label, created_at, expires_at, last_used_at FROM api_tokens WHERE token_hash = $1",
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
                user_id: UserId(SOLO_USER_ID.to_string()),
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

    async fn verify_password(&self, _user_id: &UserId, password: &str) -> Result<bool> {
        let row = sqlx::query("SELECT password_hash FROM owner WHERE id = 1")
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

    async fn set_password(&self, _user_id: &UserId, password_hash: &str) -> Result<()> {
        let now = now_unix() as i64;
        sqlx::query("UPDATE owner SET password_hash = $1, password_changed_at = $2 WHERE id = 1")
            .bind(password_hash)
            .bind(now)
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
            .context("failed to run migrations")?;
        Ok(())
    }

    async fn health_check(&self) -> Result<()> {
        sqlx::query("SELECT 1")
            .fetch_one(&self.pool)
            .await
            .context("database health check failed")?;
        Ok(())
    }

    // --- User management (not supported in server) ---

    async fn create_user(
        &self,
        _email: &str,
        _password_hash: &str,
        _display_name: &str,
    ) -> Result<UserId> {
        anyhow::bail!("user management is not supported")
    }

    async fn get_user_by_email(&self, _email: &str) -> Result<Option<UserRecord>> {
        anyhow::bail!("user management is not supported")
    }

    async fn get_user(&self, _user_id: &UserId) -> Result<Option<UserRecord>> {
        anyhow::bail!("user management is not supported")
    }

    async fn update_user(&self, _user_id: &UserId, _update: &UserUpdate) -> Result<()> {
        anyhow::bail!("user management is not supported")
    }
}
