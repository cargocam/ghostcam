<script lang="ts">
	import { untrack } from 'svelte';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import LoginPage from '$lib/components/LoginPage.svelte';
	import Sidebar from '$lib/components/layout/Sidebar.svelte';
	import Header from '$lib/components/layout/Header.svelte';
	import MobileNav from '$lib/components/layout/MobileNav.svelte';
	import SettingsDialog from '$lib/components/layout/SettingsDialog.svelte';
	import AlertsSheet from '$lib/components/alerts/AlertsSheet.svelte';
	import TimelineScrubber from '$lib/components/TimelineScrubber.svelte';
	import LiveView from '$lib/views/LiveView.svelte';
	import CameraView from '$lib/views/CameraView.svelte';
	import MapView from '$lib/views/MapView.svelte';
	import DashboardView from '$lib/views/DashboardView.svelte';

	let mobileNavOpen = $state(false);
	let settingsOpen = $state(false);
	let alertsOpen = $state(false);

	$effect(() => {
		settingsStore.applyTheme();
	});

	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(prefers-color-scheme: dark)');
		const handler = () => {
			if (settingsStore.theme === 'system') settingsStore.applyTheme();
		};
		mq.addEventListener('change', handler);
		return () => mq.removeEventListener('change', handler);
	});

	$effect(() => {
		untrack(() => transportStore.initialize());
		return () => { transportStore.destroy(); };
	});

	// Initialize scrubber globally
	$effect(() => {
		scrubberStore.initialize();
		return () => { scrubberStore.destroy(); };
	});
</script>

{#if !transportStore.authenticated}
	<LoginPage />
{:else if settingsStore.currentView === 'camera'}
	<div class="h-dvh overflow-hidden bg-black">
		<CameraView />
	</div>
{:else}
	<div class="flex h-dvh overflow-hidden bg-background">
		<Sidebar />
		<MobileNav bind:open={mobileNavOpen} />

		<div class="flex flex-1 flex-col min-w-0">
			<Header
				onMenuClick={() => (mobileNavOpen = true)}
				onSettingsClick={() => (settingsOpen = true)}
				onAlertsClick={() => (alertsOpen = !alertsOpen)}
			/>

			<main class="flex-1 overflow-hidden relative">
				<div class="absolute inset-0" class:hidden={settingsStore.currentView !== 'live'}>
					<LiveView />
				</div>
				<div class="absolute inset-0" class:hidden={settingsStore.currentView !== 'map'}>
					<MapView />
				</div>
				<div class="absolute inset-0" class:hidden={settingsStore.currentView !== 'dashboard'}>
					<DashboardView />
				</div>
			</main>

			<!-- Universal timeline scrubber — visible across all views -->
			<TimelineScrubber />
		</div>
	</div>
{/if}

<SettingsDialog bind:open={settingsOpen} />
<AlertsSheet bind:open={alertsOpen} />
