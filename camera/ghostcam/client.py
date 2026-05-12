"""HTTP client to the Ghostcam server.

Mirrors camera/client.go. Every authenticated request gets the
Authorization header from signing.py; provision is the one unauthenticated
endpoint (the camera's public key isn't registered yet).

Three operations live here:
  * post_telemetry        — POST /api/v1/cameras/<id>/telemetry
  * request_presigned_urls — POST /api/v1/cameras/<id>/presign
  * upload_segment        — PUT to a presigned S3 URL with Content-Type video/mp2t
  * provision             — POST /api/v1/cameras/provision (unauthenticated)
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any

import httpx

from ghostcam.identity import Identity
from ghostcam.signing import build_signature_header
from ghostcam.wire import (
    CameraCommand,
    LocalManifestRequest,
    PresignRequest,
    PresignResponse,
    ProvisionRequest,
    ProvisionResponse,
    TelemetryDatagram,
    TelemetryPollRequest,
    TelemetryPollResponse,
    UploadedSegment,
)

logger = logging.getLogger(__name__)


# Set at build time; "dev" disables firmware self-update.
VERSION = "dev"


class S3UploadError(Exception):
    """Raised by upload_segment when the S3 PUT returns a non-2xx status.
    Mirrors camera/client.go::S3UploadError.
    """

    def __init__(self, status_code: int) -> None:
        super().__init__(f"S3 PUT returned {status_code}")
        self.status_code = status_code

    @property
    def is_client_error(self) -> bool:
        return self.status_code // 100 == 4


@dataclass
class Client:
    """Authenticated HTTP client. Constructed once per process; the
    underlying httpx.AsyncClient is created on demand and held for the
    process lifetime.
    """

    server_url: str
    device_id: str
    identity: Identity
    timeout: float = 30.0
    _http: httpx.AsyncClient | None = None

    def __post_init__(self) -> None:
        self.server_url = self.server_url.rstrip("/")

    async def _client(self) -> httpx.AsyncClient:
        if self._http is None:
            self._http = httpx.AsyncClient(timeout=self.timeout)
        return self._http

    async def aclose(self) -> None:
        if self._http is not None:
            await self._http.aclose()
            self._http = None

    def _auth_header(self, method: str, path: str) -> str:
        return build_signature_header(method, path, self.device_id, self.identity.signing_key)

    async def _post_json(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        client = await self._client()
        url = self.server_url + path
        headers = {
            "Content-Type": "application/json",
            "Authorization": self._auth_header("POST", path),
        }
        resp = await client.post(url, json=body, headers=headers)
        if resp.status_code // 100 != 2:
            raise httpx.HTTPStatusError(
                f"HTTP POST {path} returned {resp.status_code}: {resp.text}",
                request=resp.request,
                response=resp,
            )
        data: dict[str, Any] = resp.json()
        return data

    async def post_telemetry(self, telemetry: TelemetryDatagram) -> list[CameraCommand]:
        """Convenience wrapper returning just the commands list.

        Most callers (including the integration tests) only care about
        the commands. The telemetry-poll loop needs the full response
        because of `wake_live`; it should call `post_telemetry_full`
        instead.
        """
        return (await self.post_telemetry_full(telemetry)).commands or []

    async def post_telemetry_full(
        self, telemetry: TelemetryDatagram,
    ) -> TelemetryPollResponse:
        """Full telemetry response including `wake_live` for standby mode."""
        body = TelemetryPollRequest(telemetry=telemetry, fw_version=VERSION).model_dump(
            by_alias=True, exclude_none=True
        )
        path = f"/api/v1/cameras/{self.device_id}/telemetry"
        raw = await self._post_json(path, body)
        return TelemetryPollResponse.model_validate(raw)

    async def request_presigned_urls(
        self,
        count: int,
        uploaded: list[UploadedSegment] | None = None,
        pending: list[UploadedSegment] | None = None,
    ) -> PresignResponse:
        """Request `count` presigned PUT URLs and tell the server about
        previously-completed (`uploaded`) and just-started (`pending`)
        segments. The pending list surfaces in the viewer's timeline as
        a blue placeholder until the next confirm cycle flips it solid.
        """
        body = PresignRequest(
            count=count, uploaded=uploaded, pending=pending,
        ).model_dump(by_alias=True, exclude_none=True)
        path = f"/api/v1/cameras/{self.device_id}/presign"
        raw = await self._post_json(path, body)
        return PresignResponse.model_validate(raw)

    async def upload_segment(self, presigned_url: str, data: bytes) -> None:
        client = await self._client()
        resp = await client.put(
            presigned_url, content=data, headers={"Content-Type": "video/mp2t"}
        )
        if resp.status_code // 100 != 2:
            raise S3UploadError(resp.status_code)

    async def post_local_manifest(
        self, segments: list[UploadedSegment],
    ) -> None:
        """Lazy-mode camera tells the server about segments it's
        holding locally. Server records them with uploaded_to_s3=FALSE
        so the timeline shows them; scrub triggers an upload_segments
        command to fetch the bytes on demand.

        No-op when segments is empty.
        """
        if not segments:
            return
        body = LocalManifestRequest(segments=segments).model_dump(
            by_alias=True, exclude_none=True,
        )
        path = f"/api/v1/cameras/{self.device_id}/local-manifest"
        await self._post_json(path, body)


async def provision(
    server_url: str, token: str, device_serial: str, identity: Identity
) -> ProvisionResponse:
    """First-boot enrollment. Sends the camera's public key so the server
    can derive device_id and register the key. Unauthenticated — the
    public key isn't registered yet.

    Mirrors camera/client.go::Provision.
    """
    body = ProvisionRequest(
        token=token,
        device_serial=device_serial,
        public_key=identity.public_key_hex,
        fw_version=VERSION,
    ).model_dump(by_alias=True, exclude_none=True)
    url = server_url.rstrip("/") + "/api/v1/cameras/provision"

    async with httpx.AsyncClient(timeout=30.0) as http:
        resp = await http.post(url, json=body, headers={"Content-Type": "application/json"})
        if resp.status_code // 100 != 2:
            raise httpx.HTTPStatusError(
                f"provisioning failed: {resp.status_code} — {resp.text}",
                request=resp.request,
                response=resp,
            )
        logger.info("provisioned device_id=%s", identity.device_id)
        return ProvisionResponse.model_validate(resp.json())
