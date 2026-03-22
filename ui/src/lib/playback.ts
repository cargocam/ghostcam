export function getHlsManifestUrl(
	deviceId: string,
	startTime: number,
	endTime: number,
): string {
	return `/api/v1/cameras/${encodeURIComponent(deviceId)}/playback?start=${startTime}&end=${endTime}`;
}

export function getSegmentUrl(deviceId: string, segmentId: string): string {
	return `/api/v1/cameras/${encodeURIComponent(deviceId)}/segments/${encodeURIComponent(segmentId)}`;
}
