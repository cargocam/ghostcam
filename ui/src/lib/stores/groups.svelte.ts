import type { GroupInfo } from '$lib/types.js';

class GroupStore {
	groups = $state<GroupInfo[]>([]);
	activeGroupId = $state<string | null>(null);

	setGroups(list: GroupInfo[]) {
		this.groups = list;
	}

	setActiveGroup(groupId: string | null) {
		this.activeGroupId = groupId;
	}
}

export const groupStore = new GroupStore();
