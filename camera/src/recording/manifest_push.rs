use anyhow::Result;
use server_core::frames::InboundStreamTag;

/// Push the current manifest to the server via a QUIC upload stream.
pub async fn push_manifest(connection: &quinn::Connection, manifest: &str) -> Result<()> {
    let mut stream = connection.open_uni().await?;
    stream
        .write_all(&[InboundStreamTag::Manifest as u8])
        .await?;
    stream.write_all(manifest.as_bytes()).await?;
    stream.finish()?;
    tracing::debug!("manifest pushed to server");
    Ok(())
}
