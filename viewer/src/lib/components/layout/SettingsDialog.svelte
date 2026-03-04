<script lang="ts">
	import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription } from '$lib/components/ui/sheet/index.js';
	import { Separator } from '$lib/components/ui/separator/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { Sun, Moon, Monitor, Bug } from 'lucide-svelte';

	let {
		open = $bindable(false),
	}: {
		open?: boolean;
	} = $props();
</script>

<Sheet bind:open>
	<SheetContent side="right">
		<SheetHeader>
			<SheetTitle>Settings</SheetTitle>
			<SheetDescription>Viewer preferences</SheetDescription>
		</SheetHeader>

		<div class="mt-6 space-y-6 overflow-y-auto" style="max-height: calc(100vh - 8rem);">
			<!-- Theme -->
			<div>
				<h3 class="text-sm font-medium mb-3">Theme</h3>
				<div class="flex gap-2">
					<Button
						variant={settingsStore.theme === 'light' ? 'default' : 'outline'}
						size="sm"
						onclick={() => settingsStore.setTheme('light')}
					>
						<Sun class="h-4 w-4 mr-1.5" />
						Light
					</Button>
					<Button
						variant={settingsStore.theme === 'dark' ? 'default' : 'outline'}
						size="sm"
						onclick={() => settingsStore.setTheme('dark')}
					>
						<Moon class="h-4 w-4 mr-1.5" />
						Dark
					</Button>
					<Button
						variant={settingsStore.theme === 'system' ? 'default' : 'outline'}
						size="sm"
						onclick={() => settingsStore.setTheme('system')}
					>
						<Monitor class="h-4 w-4 mr-1.5" />
						System
					</Button>
				</div>
			</div>

			<Separator />

			<!-- Debug mode -->
			<div>
				<h3 class="text-sm font-medium mb-3">Developer</h3>
				<Button
					variant={settingsStore.debugMode ? 'default' : 'outline'}
					size="sm"
					onclick={() => (settingsStore.debugMode = !settingsStore.debugMode)}
				>
					<Bug class="h-4 w-4 mr-1.5" />
					Debug Overlay
				</Button>
				<p class="text-xs text-muted-foreground mt-1.5">Show WebRTC stats on camera cards</p>
			</div>

			<Separator />

			<!-- Connection status -->
			<div>
				<h3 class="text-sm font-medium mb-2">Connection</h3>
				<div class="flex items-center gap-2 text-sm">
					<span class={transportStore.connected ? 'text-primary' : 'text-destructive'}>
						{transportStore.connected ? 'Connected' : 'Disconnected'}
					</span>
					<span class="text-xs text-muted-foreground">({transportStore.connectionState})</span>
					{#if transportStore.error}
						<span class="text-xs text-destructive">{transportStore.error}</span>
					{/if}
				</div>
			</div>
		</div>
	</SheetContent>
</Sheet>
