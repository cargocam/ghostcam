"""Integration test fixtures: testcontainers + Go server subprocess.

Spins up Postgres, Redis, MinIO via testcontainers-python, then builds
and starts the Go server as a subprocess pointed at those ports. The
server runs migrations on startup. Each test module gets a clean
admin user (created via the server's auth API) and a fresh provision
token so device IDs don't collide.

Skips the whole tree (collection-only) when:
  * --integration was not passed
  * `testcontainers` isn't installed
  * Docker daemon is unreachable (testcontainers raises on first .start())

Test isolation:
  * Each test module gets its own admin email so user-scoped state
    (cameras, segments) doesn't leak between modules.
  * Postgres + Redis + MinIO are session-scoped (one set of containers
    per pytest invocation) — fast.
  * The Go server is also session-scoped. Tests that mutate global
    state should clean up after themselves.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import time
from collections.abc import Iterator
from pathlib import Path
from typing import TYPE_CHECKING

import httpx
import pytest

if TYPE_CHECKING:
    pass


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption(
        "--integration",
        action="store_true",
        default=False,
        help="Run integration tests (requires Docker daemon + testcontainers).",
    )


def pytest_collection_modifyitems(
    config: pytest.Config, items: list[pytest.Item]
) -> None:
    if config.getoption("--integration"):
        return
    skip = pytest.mark.skip(reason="integration tests require --integration")
    for item in items:
        if "integration" in str(item.fspath):
            item.add_marker(skip)


@pytest.fixture(scope="session")
def docker_available() -> bool:
    if shutil.which("docker") is None:
        pytest.skip("docker not on PATH")
    try:
        out = subprocess.run(
            ["docker", "info"], capture_output=True, timeout=5,
        )
    except (subprocess.TimeoutExpired, OSError):
        pytest.skip("docker daemon unreachable")
    if out.returncode != 0:
        pytest.skip("docker daemon unreachable")
    return True


@pytest.fixture(scope="session")
def testcontainers_available() -> bool:
    try:
        import testcontainers.core.container  # noqa: F401
    except ImportError:
        pytest.skip("testcontainers not installed (pip install testcontainers)")
    return True


def _free_port() -> int:
    """OS-allocated free port. Used for the Go server's HTTP listener."""
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="session")
def postgres_url(docker_available: bool, testcontainers_available: bool) -> Iterator[str]:
    from testcontainers.postgres import PostgresContainer  # type: ignore[import-not-found]

    pg = PostgresContainer("postgres:16-alpine")
    pg.start()
    try:
        # Convert SQLAlchemy URL → bare postgres:// for pgx.
        url = pg.get_connection_url().replace("postgresql+psycopg2://", "postgres://")
        yield url
    finally:
        pg.stop()


@pytest.fixture(scope="session")
def redis_url(docker_available: bool, testcontainers_available: bool) -> Iterator[str]:
    from testcontainers.redis import RedisContainer  # type: ignore[import-not-found]

    rc = RedisContainer("redis:7-alpine")
    rc.start()
    try:
        host = rc.get_container_host_ip()
        port = rc.get_exposed_port(6379)
        yield f"redis://{host}:{port}"
    finally:
        rc.stop()


@pytest.fixture(scope="session")
def minio(docker_available: bool, testcontainers_available: bool) -> Iterator[dict[str, str]]:
    """MinIO + bucket. Returns {endpoint, region, bucket, access_key, secret_key}."""
    from testcontainers.minio import MinioContainer  # type: ignore[import-not-found]

    mc = MinioContainer()
    mc.start()
    try:
        config = mc.get_config()
        # Bucket must exist before the server tries to issue presigned URLs.
        # MinioContainer doesn't create one; we use the boto3 client.
        import boto3  # type: ignore[import-untyped]
        s3 = boto3.client(
            "s3",
            endpoint_url=f"http://{config['endpoint']}",
            aws_access_key_id=config["access_key"],
            aws_secret_access_key=config["secret_key"],
            region_name="us-east-1",
        )
        s3.create_bucket(Bucket="ghostcam-segments")
        yield {
            "endpoint": f"http://{config['endpoint']}",
            "bucket": "ghostcam-segments",
            "access_key": config["access_key"],
            "secret_key": config["secret_key"],
            "region": "us-east-1",
        }
    finally:
        mc.stop()


@pytest.fixture(scope="session")
def go_server_binary(tmp_path_factory: pytest.TempPathFactory) -> Path:
    """Build the Go server once per session.

    We deliberately don't ship a server binary as part of the Python wheel —
    these tests only run when the developer has the full repo checked out.
    """
    repo_root = Path(__file__).resolve().parents[3]
    if not (repo_root / "server" / "main.go").exists():
        pytest.skip(f"server source not found under {repo_root}")
    out = tmp_path_factory.mktemp("server-bin") / "ghostcam-server"
    proc = subprocess.run(
        ["go", "build", "-o", str(out), "./server"],
        cwd=repo_root,
        capture_output=True,
    )
    if proc.returncode != 0:
        pytest.fail(
            f"go build failed: {proc.stderr.decode(errors='replace')}"
        )
    return out


@pytest.fixture(scope="session")
def server(
    go_server_binary: Path,
    postgres_url: str,
    redis_url: str,
    minio: dict[str, str],
) -> Iterator[dict[str, str]]:
    """Start the Go server as a subprocess. Yields {url, admin_email,
    admin_password} once /healthz returns 200.

    The server runs migrations on first connect, so we don't need a
    separate setup step.
    """
    port = _free_port()
    admin_email = "integration@test.local"
    admin_password = "integration-password-9000"
    env = {
        **os.environ,
        "GHOSTCAM_DATABASE_URL": postgres_url,
        "GHOSTCAM_REDIS_URL": redis_url,
        "GHOSTCAM_DATA_DIR": "/tmp/ghostcam-integration-server",
        "GHOSTCAM_ADMIN_EMAIL": admin_email,
        "GHOSTCAM_ADMIN_PASSWORD": admin_password,
        "GHOSTCAM_PORT": str(port),
        "GHOSTCAM_S3_ENDPOINT": minio["endpoint"],
        "GHOSTCAM_S3_BUCKET": minio["bucket"],
        "GHOSTCAM_S3_REGION": minio["region"],
        "AWS_ACCESS_KEY_ID": minio["access_key"],
        "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
        "GHOSTCAM_PUBLIC_IP": "127.0.0.1",
        # Stripe + Resend left unset — server logs warn but starts.
    }
    proc = subprocess.Popen(
        [str(go_server_binary)],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    base_url = f"http://127.0.0.1:{port}"
    deadline = time.time() + 30.0
    try:
        while time.time() < deadline:
            try:
                r = httpx.get(f"{base_url}/healthz", timeout=1.0)
                if r.status_code == 200:
                    break
            except httpx.HTTPError:
                pass
            if proc.poll() is not None:
                pytest.fail(
                    "server exited during startup: "
                    + (proc.stdout.read().decode(errors="replace") if proc.stdout else ""),
                )
            time.sleep(0.5)
        else:
            pytest.fail("server did not become ready within 30s")

        yield {
            "url": base_url,
            "admin_email": admin_email,
            "admin_password": admin_password,
        }
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5.0)
        except subprocess.TimeoutExpired:
            proc.kill()


@pytest.fixture
def admin_session(server: dict[str, str]) -> httpx.Client:
    """A logged-in HTTP client with the admin's session cookie set."""
    client = httpx.Client(base_url=server["url"], timeout=10.0)
    r = client.post(
        "/api/v1/auth/login",
        json={
            "email": server["admin_email"],
            "password": server["admin_password"],
        },
    )
    r.raise_for_status()
    return client


@pytest.fixture
def provision_token(admin_session: httpx.Client) -> str:
    """Generate a fresh provision token via the admin API."""
    r = admin_session.post("/api/v1/cameras", json={})
    r.raise_for_status()
    body = r.json()
    return body["token"]
