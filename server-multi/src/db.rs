use anyhow::{Context, Result};
use async_trait::async_trait;
use ghostcam::types::{CertFingerprint, DeviceId, SessionId, TokenId, UserId};
use server_core::auth;
use server_core::db::{
    ApiTokenRecord, CameraRecord, CameraUpdate, Database, NewApiToken, NewCameraRecord,
    NewEnrollmentToken, NewSession, SessionRecord, UserRecord, UserUpdate,
};
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::{PgPool, Row};
use std::str::FromStr;
use std::time::{SystemTime, UNIX_EPOCH};

fn now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs()
}

fn to_timestamptz(epoch: u64) -> chrono::DateTime<chrono::Utc> {
    chrono::DateTime::from_timestamp(epoch as i64, 0).unwrap_or_default()
}

fn from_timestamptz(dt: chrono::DateTime<chrono::Utc>) -> u64 {
    dt.timestamp() as u64
}

pub struct PostgresDatabase {
    pool: PgPool,
}

impl PostgresDatabase {
    pub async fn connect(database_url: &str) -> Result<Self> {
        let options = PgConnectOptions::from_str(database_url)?;

        let pool = PgPoolOptions::new()
            .max_connections(10)
            .connect_with(options)
            .await
            .context("failed to connect to Postgres")?;

        let db = Self { pool };
        db.migrate().await?;
        Ok(db)
    }

    /// Initialize the HMAC secret in the config table if not present.
    pub async fn initialize(&self) -> Result<()> {
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

        Ok(())
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

        Ok(row.map(|r| {
            let device_id: uuid::Uuid = r.get("device_id");
            let user_id: uuid::Uuid = r.get("user_id");
            let enrolled_at: chrono::DateTime<chrono::Utc> = r.get("enrolled_at");
            let last_seen_at: Option<chrono::DateTime<chrono::Utc>> = r.get("last_seen_at");
            CameraRecord {
                device_id: DeviceId(device_id.to_string()),
                user_id: UserId(user_id.to_string()),
                cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
                display_name: r.get("display_name"),
                enrolled_at: from_timestamptz(enrolled_at),
                last_seen_at: last_seen_at.map(from_timestamptz),
                notes: r.get("notes"),
            }
        }))
    }

    async fn get_camera(&self, device_id: &DeviceId) -> Result<Option<CameraRecord>> {
        let uuid = uuid::Uuid::parse_str(&device_id.0).context("invalid device_id UUID")?;

        let row = sqlx::query(
            "SELECT device_id, user_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras WHERE device_id = $1",
        )
        .bind(uuid)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| {
            let device_id: uuid::Uuid = r.get("device_id");
            let user_id: uuid::Uuid = r.get("user_id");
            let enrolled_at: chrono::DateTime<chrono::Utc> = r.get("enrolled_at");
            let last_seen_at: Option<chrono::DateTime<chrono::Utc>> = r.get("last_seen_at");
            CameraRecord {
                device_id: DeviceId(device_id.to_string()),
                user_id: UserId(user_id.to_string()),
                cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
                display_name: r.get("display_name"),
                enrolled_at: from_timestamptz(enrolled_at),
                last_seen_at: last_seen_at.map(from_timestamptz),
                notes: r.get("notes"),
            }
        }))
    }

    async fn list_cameras(&self, user_id: &UserId) -> Result<Vec<CameraRecord>> {
        let uuid = uuid::Uuid::parse_str(&user_id.0).context("invalid user_id UUID")?;

        let rows = sqlx::query(
            "SELECT device_id, user_id, cert_fingerprint, display_name, enrolled_at, last_seen_at, notes FROM cameras WHERE user_id = $1 ORDER BY enrolled_at",
        )
        .bind(uuid)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .into_iter()
            .map(|r| {
                let device_id: uuid::Uuid = r.get("device_id");
                let user_id: uuid::Uuid = r.get("user_id");
                let enrolled_at: chrono::DateTime<chrono::Utc> = r.get("enrolled_at");
                let last_seen_at: Option<chrono::DateTime<chrono::Utc>> = r.get("last_seen_at");
                CameraRecord {
                    device_id: DeviceId(device_id.to_string()),
                    user_id: UserId(user_id.to_string()),
                    cert_fingerprint: CertFingerprint(r.get("cert_fingerprint")),
                    display_name: r.get("display_name"),
                    enrolled_at: from_timestamptz(enrolled_at),
                    last_seen_at: last_seen_at.map(from_timestamptz),
                    notes: r.get("notes"),
                }
            })
            .collect())
    }

    async fn create_camera(&self, record: &NewCameraRecord) -> Result<CameraRecord> {
        let user_uuid = uuid::Uuid::parse_str(&record.user_id.0).context("invalid user_id UUID")?;

        let row = sqlx::query(
            "INSERT INTO cameras (user_id, cert_fingerprint, display_name) VALUES ($1, $2, $3) RETURNING device_id, enrolled_at",
        )
        .bind(user_uuid)
        .bind(&record.cert_fingerprint.0)
        .bind(&record.display_name)
        .fetch_one(&self.pool)
        .await?;

        let device_id: uuid::Uuid = row.get("device_id");
        let enrolled_at: chrono::DateTime<chrono::Utc> = row.get("enrolled_at");

        Ok(CameraRecord {
            device_id: DeviceId(device_id.to_string()),
            user_id: record.user_id.clone(),
            cert_fingerprint: record.cert_fingerprint.clone(),
            display_name: record.display_name.clone(),
            enrolled_at: from_timestamptz(enrolled_at),
            last_seen_at: None,
            notes: None,
        })
    }

    async fn update_camera(&self, device_id: &DeviceId, update: &CameraUpdate) -> Result<()> {
        let uuid = uuid::Uuid::parse_str(&device_id.0).context("invalid device_id UUID")?;

        if let Some(ref name) = update.display_name {
            sqlx::query("UPDATE cameras SET display_name = $1 WHERE device_id = $2")
                .bind(name)
                .bind(uuid)
                .execute(&self.pool)
                .await?;
        }
        if let Some(ref notes) = update.notes {
            sqlx::query("UPDATE cameras SET notes = $1 WHERE device_id = $2")
                .bind(notes)
                .bind(uuid)
                .execute(&self.pool)
                .await?;
        }
        Ok(())
    }

    async fn delete_camera(&self, device_id: &DeviceId) -> Result<()> {
        let uuid = uuid::Uuid::parse_str(&device_id.0).context("invalid device_id UUID")?;

        // Clear enrollment token references first
        sqlx::query("UPDATE enrollment_tokens SET claimed_by = NULL, claimed_at = NULL WHERE claimed_by = $1")
            .bind(uuid)
            .execute(&self.pool)
            .await?;

        sqlx::query("DELETE FROM cameras WHERE device_id = $1")
            .bind(uuid)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn update_last_seen(&self, device_id: &DeviceId) -> Result<()> {
        let uuid = uuid::Uuid::parse_str(&device_id.0).context("invalid device_id UUID")?;

        sqlx::query("UPDATE cameras SET last_seen_at = now() WHERE device_id = $1")
            .bind(uuid)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    // --- Enrollment tokens ---

    async fn create_enrollment_token(&self, token: &NewEnrollmentToken) -> Result<()> {
        let user_uuid = uuid::Uuid::parse_str(&token.user_id.0).context("invalid user_id UUID")?;
        let expires_at = to_timestamptz(token.expires_at);

        sqlx::query("INSERT INTO enrollment_tokens (jti, user_id, expires_at) VALUES ($1, $2, $3)")
            .bind(&token.jti)
            .bind(user_uuid)
            .bind(expires_at)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn claim_enrollment_token(&self, jti: &str, device_id: &DeviceId) -> Result<bool> {
        let device_uuid = uuid::Uuid::parse_str(&device_id.0).context("invalid device_id UUID")?;

        let result = sqlx::query(
            "UPDATE enrollment_tokens SET claimed_by = $1, claimed_at = now() WHERE jti = $2 AND claimed_by IS NULL AND expires_at > now()",
        )
        .bind(device_uuid)
        .bind(jti)
        .execute(&self.pool)
        .await?;

        Ok(result.rows_affected() > 0)
    }

    async fn cleanup_expired_tokens(&self) -> Result<u64> {
        let result = sqlx::query(
            "DELETE FROM enrollment_tokens WHERE expires_at < now() AND claimed_by IS NULL",
        )
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected())
    }

    // --- Sessions ---

    async fn create_session(&self, session: &NewSession) -> Result<()> {
        let user_uuid =
            uuid::Uuid::parse_str(&session.user_id.0).context("invalid user_id UUID")?;
        let expires_at = to_timestamptz(now_unix() + 86400 * 30);

        sqlx::query(
            "INSERT INTO sessions (session_id, user_id, expires_at, user_agent, ip_address) VALUES ($1, $2, $3, $4, $5::INET)",
        )
        .bind(&session.session_id.0)
        .bind(user_uuid)
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

        Ok(row.map(|r| {
            let user_id: uuid::Uuid = r.get("user_id");
            let created_at: chrono::DateTime<chrono::Utc> = r.get("created_at");
            let expires_at: chrono::DateTime<chrono::Utc> = r.get("expires_at");
            let last_active_at: Option<chrono::DateTime<chrono::Utc>> = r.get("last_active_at");
            SessionRecord {
                session_id: SessionId(r.get("session_id")),
                user_id: UserId(user_id.to_string()),
                created_at: from_timestamptz(created_at),
                expires_at: from_timestamptz(expires_at),
                last_active_at: last_active_at.map(from_timestamptz),
            }
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
        sqlx::query("UPDATE sessions SET last_active_at = now() WHERE session_id = $1")
            .bind(&session_id.0)
            .execute(&self.pool)
            .await?;
        Ok(())
    }

    async fn cleanup_expired_sessions(&self) -> Result<u64> {
        let result = sqlx::query("DELETE FROM sessions WHERE expires_at < now()")
            .execute(&self.pool)
            .await?;
        Ok(result.rows_affected())
    }

    // --- API tokens ---

    async fn create_api_token(&self, token: &NewApiToken) -> Result<()> {
        let user_uuid = uuid::Uuid::parse_str(&token.user_id.0).context("invalid user_id UUID")?;
        let expires_at = token.expires_at.map(to_timestamptz);

        sqlx::query(
            "INSERT INTO api_tokens (token_id, user_id, token_hash, label, expires_at) VALUES ($1, $2, $3, $4, $5)",
        )
        .bind(&token.token_id.0)
        .bind(user_uuid)
        .bind(&token.token_hash)
        .bind(&token.label)
        .bind(expires_at)
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    async fn list_api_tokens(&self, user_id: &UserId) -> Result<Vec<ApiTokenRecord>> {
        let uuid = uuid::Uuid::parse_str(&user_id.0).context("invalid user_id UUID")?;

        let rows = sqlx::query(
            "SELECT token_id, user_id, label, created_at, expires_at, last_used_at FROM api_tokens WHERE user_id = $1 ORDER BY created_at",
        )
        .bind(uuid)
        .fetch_all(&self.pool)
        .await?;

        Ok(rows
            .into_iter()
            .map(|r| {
                let user_id: uuid::Uuid = r.get("user_id");
                let created_at: chrono::DateTime<chrono::Utc> = r.get("created_at");
                let expires_at: Option<chrono::DateTime<chrono::Utc>> = r.get("expires_at");
                let last_used_at: Option<chrono::DateTime<chrono::Utc>> = r.get("last_used_at");
                ApiTokenRecord {
                    token_id: TokenId(r.get("token_id")),
                    user_id: UserId(user_id.to_string()),
                    label: r.get("label"),
                    created_at: from_timestamptz(created_at),
                    expires_at: expires_at.map(from_timestamptz),
                    last_used_at: last_used_at.map(from_timestamptz),
                }
            })
            .collect())
    }

    async fn verify_api_token(&self, token_hash: &str) -> Result<Option<ApiTokenRecord>> {
        let row = sqlx::query(
            "UPDATE api_tokens SET last_used_at = now() WHERE token_hash = $1 RETURNING token_id, user_id, label, created_at, expires_at, last_used_at",
        )
        .bind(token_hash)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| {
            let user_id: uuid::Uuid = r.get("user_id");
            let created_at: chrono::DateTime<chrono::Utc> = r.get("created_at");
            let expires_at: Option<chrono::DateTime<chrono::Utc>> = r.get("expires_at");
            let last_used_at: Option<chrono::DateTime<chrono::Utc>> = r.get("last_used_at");
            ApiTokenRecord {
                token_id: TokenId(r.get("token_id")),
                user_id: UserId(user_id.to_string()),
                label: r.get("label"),
                created_at: from_timestamptz(created_at),
                expires_at: expires_at.map(from_timestamptz),
                last_used_at: last_used_at.map(from_timestamptz),
            }
        }))
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
        let uuid = uuid::Uuid::parse_str(&user_id.0).context("invalid user_id UUID")?;

        let row = sqlx::query("SELECT password_hash FROM users WHERE user_id = $1")
            .bind(uuid)
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
        let uuid = uuid::Uuid::parse_str(&user_id.0).context("invalid user_id UUID")?;

        sqlx::query("UPDATE users SET password_hash = $1 WHERE user_id = $2")
            .bind(password_hash)
            .bind(uuid)
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
        // Use advisory lock to prevent concurrent migrations in multi-instance deploys
        sqlx::query("SELECT pg_advisory_lock(42)")
            .execute(&self.pool)
            .await?;

        let result = sqlx::raw_sql(include_str!("../migrations/001_initial.sql"))
            .execute(&self.pool)
            .await;

        // Always release the lock
        sqlx::query("SELECT pg_advisory_unlock(42)")
            .execute(&self.pool)
            .await?;

        result.context("failed to run migrations")?;
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
        let email_lower = email.to_lowercase();
        let row = sqlx::query(
            "INSERT INTO users (email, password_hash, display_name) VALUES ($1, $2, $3) RETURNING user_id",
        )
        .bind(&email_lower)
        .bind(password_hash)
        .bind(display_name)
        .fetch_one(&self.pool)
        .await?;

        let user_id: uuid::Uuid = row.get("user_id");
        Ok(UserId(user_id.to_string()))
    }

    async fn get_user_by_email(&self, email: &str) -> Result<Option<UserRecord>> {
        let email_lower = email.to_lowercase();
        let row = sqlx::query(
            "SELECT user_id, email, display_name, created_at, verified_at, disabled_at FROM users WHERE email = $1",
        )
        .bind(&email_lower)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| {
            let user_id: uuid::Uuid = r.get("user_id");
            let created_at: chrono::DateTime<chrono::Utc> = r.get("created_at");
            let verified_at: Option<chrono::DateTime<chrono::Utc>> = r.get("verified_at");
            let disabled_at: Option<chrono::DateTime<chrono::Utc>> = r.get("disabled_at");
            UserRecord {
                user_id: UserId(user_id.to_string()),
                email: r.get("email"),
                display_name: r.get("display_name"),
                created_at: from_timestamptz(created_at),
                verified_at: verified_at.map(from_timestamptz),
                disabled_at: disabled_at.map(from_timestamptz),
            }
        }))
    }

    async fn get_user(&self, user_id: &UserId) -> Result<Option<UserRecord>> {
        let uuid = uuid::Uuid::parse_str(&user_id.0).context("invalid user_id UUID")?;

        let row = sqlx::query(
            "SELECT user_id, email, display_name, created_at, verified_at, disabled_at FROM users WHERE user_id = $1",
        )
        .bind(uuid)
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|r| {
            let user_id: uuid::Uuid = r.get("user_id");
            let created_at: chrono::DateTime<chrono::Utc> = r.get("created_at");
            let verified_at: Option<chrono::DateTime<chrono::Utc>> = r.get("verified_at");
            let disabled_at: Option<chrono::DateTime<chrono::Utc>> = r.get("disabled_at");
            UserRecord {
                user_id: UserId(user_id.to_string()),
                email: r.get("email"),
                display_name: r.get("display_name"),
                created_at: from_timestamptz(created_at),
                verified_at: verified_at.map(from_timestamptz),
                disabled_at: disabled_at.map(from_timestamptz),
            }
        }))
    }

    async fn update_user(&self, user_id: &UserId, update: &UserUpdate) -> Result<()> {
        let uuid = uuid::Uuid::parse_str(&user_id.0).context("invalid user_id UUID")?;

        if let Some(ref email) = update.email {
            let email_lower = email.to_lowercase();
            sqlx::query("UPDATE users SET email = $1 WHERE user_id = $2")
                .bind(&email_lower)
                .bind(uuid)
                .execute(&self.pool)
                .await?;
        }
        if let Some(ref name) = update.display_name {
            sqlx::query("UPDATE users SET display_name = $1 WHERE user_id = $2")
                .bind(name)
                .bind(uuid)
                .execute(&self.pool)
                .await?;
        }
        if let Some(ref hash) = update.password_hash {
            sqlx::query("UPDATE users SET password_hash = $1 WHERE user_id = $2")
                .bind(hash)
                .bind(uuid)
                .execute(&self.pool)
                .await?;
        }
        Ok(())
    }
}
