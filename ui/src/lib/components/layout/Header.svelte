<script lang="ts">
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { ToggleGroup, ToggleGroupItem } from '$lib/components/ui/toggle-group/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { Video, Map, Activity, LayoutGrid, LayoutDashboard, MapPin, Tag, MonitorPlay, Menu, Settings, Bell } from 'lucide-svelte';
	import AlertBadge from '$lib/components/alerts/AlertBadge.svelte';
	import GroupSelector from '$lib/components/GroupSelector.svelte';
	import type { ViewMode, MarkerMode } from '$lib/types.js';

	let {
		onMenuClick,
		onSettingsClick,
		onAlertsClick,
	}: {
		onMenuClick?: () => void;
		onSettingsClick?: () => void;
		onAlertsClick?: () => void;
	} = $props();
</script>

<header class="flex items-center justify-between gap-2 h-12 px-2 sm:px-4 border-b bg-background/95 backdrop-blur supports-[backdrop-filter]:bg-background/60">
	<!-- Left: mobile menu + branding -->
	<div class="flex items-center gap-2 sm:gap-3 min-w-0">
		<Button variant="ghost" size="icon" class="md:hidden shrink-0 -ml-1" onclick={onMenuClick}>
			<Menu class="h-5 w-5" />
		</Button>
		<span class="hidden sm:inline text-sm font-bold tracking-widest text-primary">GHOSTCAM</span>
	</div>

	<!-- Center: view toggle + group selector -->
	<div class="flex items-center gap-2 min-w-0">
		<ToggleGroup bind:value={
			() => settingsStore.currentView,
			(v) => settingsStore.setView(v as ViewMode)
		}>
			<ToggleGroupItem value="live">
				<Video class="h-3.5 w-3.5 sm:mr-1" />
				<span class="text-xs hidden sm:inline">VIDEO</span>
			</ToggleGroupItem>
			<ToggleGroupItem value="map">
				<Map class="h-3.5 w-3.5 sm:mr-1" />
				<span class="text-xs hidden sm:inline">MAP</span>
			</ToggleGroupItem>
			<ToggleGroupItem value="dashboard">
				<Activity class="h-3.5 w-3.5 sm:mr-1" />
				<span class="text-xs hidden sm:inline">STATS</span>
			</ToggleGroupItem>
		</ToggleGroup>

		<GroupSelector />
	</div>

	<!-- Right: context toggles + status + settings -->
	<div class="flex items-center gap-1 sm:gap-2 shrink-0">
		{#if settingsStore.currentView === 'live'}
			<ToggleGroup bind:value={
				() => settingsStore.gridLayout,
				(v) => settingsStore.setGridLayout(v as 'auto' | '1+5')
			} class="hidden lg:inline-flex">
				<ToggleGroupItem value="auto">
					<LayoutGrid class="h-3.5 w-3.5" />
				</ToggleGroupItem>
				<ToggleGroupItem value="1+5">
					<LayoutDashboard class="h-3.5 w-3.5" />
				</ToggleGroupItem>
			</ToggleGroup>
		{/if}

		{#if settingsStore.currentView === 'map'}
			<ToggleGroup bind:value={
				() => settingsStore.markerMode,
				(v) => settingsStore.setMarkerMode(v as MarkerMode)
			} class="hidden lg:inline-flex">
				<ToggleGroupItem value="dot">
					<MapPin class="h-3.5 w-3.5" />
				</ToggleGroupItem>
				<ToggleGroupItem value="detailed">
					<Tag class="h-3.5 w-3.5" />
				</ToggleGroupItem>
				<ToggleGroupItem value="pip">
					<MonitorPlay class="h-3.5 w-3.5" />
				</ToggleGroupItem>
			</ToggleGroup>
		{/if}

		<div class="hidden sm:flex items-center gap-1.5 text-xs text-muted-foreground">
			<span class={transportStore.connected ? 'text-primary' : 'text-destructive'}>
				{cameraStore.onlineCount}
			</span>
			<span>online</span>
		</div>

		<Button variant="ghost" size="icon" class="relative" onclick={onAlertsClick}>
			<Bell class="h-4 w-4" />
			<span class="absolute -top-0.5 -right-0.5">
				<AlertBadge />
			</span>
		</Button>

		<Button variant="ghost" size="icon" class="-mr-1 sm:mr-0" onclick={onSettingsClick}>
			<Settings class="h-4 w-4" />
		</Button>
	</div>
</header>
