/** Format a bitrate in kbps to a human-readable string */
export function formatBitrate(kbps: number): string {
	if (kbps >= 1000) return `${(kbps / 1000).toFixed(1)} Mbps`;
	if (kbps > 0) return `${kbps.toFixed(0)} kbps`;
	return '0 kbps';
}

/** Compact bitrate format for overlays (e.g. "1.2M", "45k") */
export function formatBitrateCompact(kbps: number): string {
	if (kbps >= 1000) return `${(kbps / 1000).toFixed(1)}M`;
	if (kbps > 0) return `${kbps.toFixed(0)}k`;
	return '';
}

/** Format seconds of uptime to a human-readable string */
export function formatUptime(secs: number): string {
	const d = Math.floor(secs / 86400);
	const h = Math.floor((secs % 86400) / 3600);
	const m = Math.floor((secs % 3600) / 60);
	if (d > 0) return `${d}d ${h}h ${m}m`;
	return `${h}h ${m}m`;
}
