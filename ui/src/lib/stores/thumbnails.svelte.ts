/** Stores latest video frame thumbnails per source for use in map PiP markers. */

let frames = $state<Record<string, string>>({});

export const thumbnailStore = {
	set(sourceId: string, dataUrl: string) {
		frames[sourceId] = dataUrl;
	},
};
