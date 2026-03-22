export type SseEvent =
	| { type: 'camera_online'; device_id: string }
	| { type: 'camera_offline'; device_id: string };

export function connectSse(
	onEvent: (event: SseEvent) => void,
	onOpen: () => void,
	onError: () => void,
): EventSource {
	const source = new EventSource('/events', { withCredentials: true });

	source.addEventListener('camera_online', (e) => {
		try {
			const data = JSON.parse((e as MessageEvent).data);
			onEvent({ type: 'camera_online', ...data });
		} catch {}
	});

	source.addEventListener('camera_offline', (e) => {
		try {
			const data = JSON.parse((e as MessageEvent).data);
			onEvent({ type: 'camera_offline', ...data });
		} catch {}
	});

	source.onopen = () => {
		onOpen();
	};

	source.onerror = () => {
		onError();
	};

	return source;
}
