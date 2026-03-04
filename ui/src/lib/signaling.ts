import type { GroupInfo, CameraInfo } from '$lib/types.js';

const API_BASE = '/api/v1';

export class SignalingClient {
	private apiKey: string;

	constructor(apiKey: string = 'dev-key') {
		this.apiKey = apiKey;
	}

	private headers(): HeadersInit {
		return {
			'Content-Type': 'application/json',
			Authorization: `Bearer ${this.apiKey}`,
		};
	}

	async watch(groupId: string, sdpOffer: string): Promise<{ session_id: string; sdp_answer: string }> {
		const res = await fetch(`${API_BASE}/watch/${encodeURIComponent(groupId)}`, {
			method: 'POST',
			headers: this.headers(),
			body: JSON.stringify({ sdp_offer: sdpOffer }),
		});
		if (!res.ok) throw new Error(`watch failed: ${res.status}`);
		return res.json();
	}

	async endSession(sessionId: string): Promise<void> {
		await fetch(`${API_BASE}/session/${sessionId}`, {
			method: 'DELETE',
			headers: this.headers(),
		});
	}

	async trickleIce(sessionId: string, candidate: string): Promise<void> {
		await fetch(`${API_BASE}/session/${sessionId}/ice`, {
			method: 'POST',
			headers: this.headers(),
			body: JSON.stringify({ candidate }),
		});
	}

	async listGroups(): Promise<GroupInfo[]> {
		const res = await fetch(`${API_BASE}/groups`, {
			headers: this.headers(),
		});
		if (!res.ok) throw new Error(`listGroups failed: ${res.status}`);
		return res.json();
	}

	async listCamerasInGroup(groupId: string): Promise<CameraInfo[]> {
		const res = await fetch(`${API_BASE}/groups/${encodeURIComponent(groupId)}/cameras`, {
			headers: this.headers(),
		});
		if (!res.ok) throw new Error(`listCameras failed: ${res.status}`);
		return res.json();
	}
}
