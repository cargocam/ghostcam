/** Stable per-camera colors (oklch for consistent perceptual brightness). */
export const CAMERA_COLORS = [
	'oklch(0.72 0.15 160)', // teal
	'oklch(0.72 0.15 250)', // blue
	'oklch(0.72 0.15 30)', // orange
	'oklch(0.72 0.15 310)', // purple
	'oklch(0.72 0.15 90)', // yellow-green
	'oklch(0.72 0.15 200)', // cyan
];

/** Get a stable color for a camera by its index in the camera list. */
export function cameraColor(index: number): string {
	return CAMERA_COLORS[index % CAMERA_COLORS.length];
}
