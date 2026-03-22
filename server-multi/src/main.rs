use server_multi::db::PostgresDatabase;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt::init();

    let database_url = std::env::var("DATABASE_URL").expect("DATABASE_URL must be set");

    let db = PostgresDatabase::connect(&database_url).await?;
    db.initialize().await?;

    tracing::info!("database initialized");

    // Server logic will be added in later plans
    Ok(())
}
