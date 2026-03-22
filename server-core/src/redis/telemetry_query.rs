use anyhow::Result;
use ghostcam::types::DeviceId;
use redis::streams::StreamRangeReply;
use serde::Serialize;

use super::connection::RedisManager;
use super::telemetry::{fields_to_entry, TelemetryEntry};

const TELEMETRY_KEY_PREFIX: &str = "telemetry:";
const MAX_LIMIT: usize = 3600;
const DEFAULT_LIMIT: usize = 3600;

/// Paginated telemetry response.
#[derive(Debug, Serialize)]
pub struct TelemetryPage {
    pub entries: Vec<TelemetryEntry>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub next_cursor: Option<String>,
}

/// Fetch the latest telemetry entry for a camera.
pub async fn get_latest(
    redis: &RedisManager,
    device_id: &DeviceId,
) -> Result<Option<TelemetryEntry>> {
    let Some(mut conn) = redis.get_conn().await else {
        anyhow::bail!("redis unavailable");
    };

    let key = format!("{}{}", TELEMETRY_KEY_PREFIX, device_id.0);

    let reply: StreamRangeReply = redis::cmd("XREVRANGE")
        .arg(&key)
        .arg("+")
        .arg("-")
        .arg("COUNT")
        .arg(1)
        .query_async(&mut conn)
        .await?;

    if let Some(entry) = reply.ids.first() {
        let fields: Vec<(String, String)> = entry
            .map
            .iter()
            .map(|(k, v)| {
                let val = match v {
                    redis::Value::BulkString(bytes) => String::from_utf8_lossy(bytes).to_string(),
                    redis::Value::SimpleString(s) => s.clone(),
                    redis::Value::Int(i) => i.to_string(),
                    _ => String::new(),
                };
                (k.clone(), val)
            })
            .collect();

        let parsed = fields_to_entry(&fields)?;
        Ok(Some(parsed))
    } else {
        Ok(None)
    }
}

/// Fetch telemetry entries within a time range with cursor-based pagination.
pub async fn query_range(
    redis: &RedisManager,
    device_id: &DeviceId,
    from_ts: u64,
    to_ts: u64,
    cursor: Option<&str>,
    limit: Option<usize>,
) -> Result<TelemetryPage> {
    let Some(mut conn) = redis.get_conn().await else {
        anyhow::bail!("redis unavailable");
    };

    let limit = limit.unwrap_or(DEFAULT_LIMIT).min(MAX_LIMIT);
    let key = format!("{}{}", TELEMETRY_KEY_PREFIX, device_id.0);

    let start = cursor
        .map(ToOwned::to_owned)
        .unwrap_or_else(|| format!("{}-0", from_ts));
    let batch_size = limit * 2; // Over-fetch to account for ts filtering

    let reply: StreamRangeReply = redis::cmd("XRANGE")
        .arg(&key)
        .arg(&start)
        .arg("+")
        .arg("COUNT")
        .arg(batch_size)
        .query_async(&mut conn)
        .await?;

    let mut entries = Vec::with_capacity(limit);
    let mut last_id = None;

    for entry in &reply.ids {
        let fields: Vec<(String, String)> = entry
            .map
            .iter()
            .map(|(k, v)| {
                let val = match v {
                    redis::Value::BulkString(bytes) => String::from_utf8_lossy(bytes).to_string(),
                    redis::Value::SimpleString(s) => s.clone(),
                    redis::Value::Int(i) => i.to_string(),
                    _ => String::new(),
                };
                (k.clone(), val)
            })
            .collect();

        if let Ok(parsed) = fields_to_entry(&fields) {
            if parsed.ts >= from_ts && parsed.ts <= to_ts {
                entries.push(parsed);
                last_id = Some(entry.id.clone());

                if entries.len() >= limit {
                    break;
                }
            }
        }
    }

    // If we filled the limit and there might be more entries, provide a cursor
    let next_cursor = if entries.len() >= limit && reply.ids.len() >= batch_size {
        last_id.map(|id| {
            // Increment the sequence to avoid returning the same entry
            // Redis stream IDs are "timestamp-seq", increment seq by 1
            if let Some(dash_pos) = id.rfind('-') {
                let (ts_part, seq_part) = id.split_at(dash_pos);
                if let Ok(seq) = seq_part[1..].parse::<u64>() {
                    return format!("{}-{}", ts_part, seq + 1);
                }
            }
            id
        })
    } else {
        None
    };

    Ok(TelemetryPage {
        entries,
        next_cursor,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn limit_clamped_to_max() {
        let clamped = Some(10000usize).unwrap_or(DEFAULT_LIMIT).min(MAX_LIMIT);
        assert_eq!(clamped, 3600);
    }

    #[test]
    fn limit_default() {
        let clamped = None::<usize>.unwrap_or(DEFAULT_LIMIT).min(MAX_LIMIT);
        assert_eq!(clamped, 3600);
    }
}
