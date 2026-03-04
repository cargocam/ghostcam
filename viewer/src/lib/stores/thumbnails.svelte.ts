/** Stores latest video frame thumbnails per source for use in map PiP markers. */

let frames = $state<Record<string, string>>({});

export const thumbnailStore = {
	get frames() {
		return frames;
	},

	set(sourceId: string, dataUrl: string) {
		frames[sourceId] = dataUrl;
	},

	get(sourceId: string): string | undefined {
		return frames[sourceId];
	},

	remove(sourceId: string) {
		delete frames[sourceId];
	},
};
