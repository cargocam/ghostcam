<script lang="ts">
	import { untrack } from 'svelte';
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { settingsStore } from '$lib/stores/settings.svelte.js';
	import { scrubberStore } from '$lib/stores/scrubber.svelte.js';
	import LoginPage from '$lib/components/LoginPage.svelte';
	import VerifyEmailPage from '$lib/components/VerifyEmailPage.svelte';
	import ResetPasswordPage from '$lib/components/ResetPasswordPage.svelte';
	import EmailChangeConfirmPage from '$lib/components/EmailChangeConfirmPage.svelte';
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
	import AdminView from '$lib/views/AdminView.svelte';

	let mobileNavOpen = $state(false);
	let settingsOpen = $state(false);
	let alertsOpen = $state(false);

	// Public routes accessible without authentication (email links)
	const publicRoutes = ['/verify-email', '/reset-password', '/email-change-confirm'] as const;
	type PublicRoute = typeof publicRoutes[number];
	let currentPublicRoute = $state<PublicRoute | null>(null);

	$effect(() => {
		const path = window.location.pathname;
		const match = publicRoutes.find((r) => path === r);
		currentPublicRoute = match ?? null;
	});

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

{#if currentPublicRoute === '/verify-email'}
	<VerifyEmailPage />
{:else if currentPublicRoute === '/reset-password'}
	<ResetPasswordPage />
{:else if currentPublicRoute === '/email-change-confirm'}
	<EmailChangeConfirmPage />
{:else if !transportStore.initialized}
	<div class="flex h-screen-stable items-center justify-center bg-background">
		<div class="flex flex-col items-center gap-4">
			<img src="/icon.svg" alt="" class="h-16 w-16 animate-pulse" />
			<h1 class="text-2xl font-bold tracking-tight">Ghostcam</h1>
		</div>
	</div>
{:else if !transportStore.authenticated}
	<LoginPage />
{:else if settingsStore.currentView === 'camera'}
	<div class="app-root h-screen-stable overflow-hidden bg-black">
		<CameraView />
	</div>
{:else if settingsStore.currentView === 'admin'}
	<!-- Admin view is a top-level surface, not a tab inside main, so the
	     sidebar / scrubber / alerts stay out of the way. A single back
	     arrow in the view itself returns to live. -->
	<div class="app-root h-screen-stable overflow-hidden bg-background">
		<AdminView />
	</div>
{:else}
	<div class="app-root flex h-screen-stable overflow-hidden bg-background">
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

<style>
	/* Pad the top (notch) and bottom (home indicator) on devices with
	   viewport-fit=cover so fixed-height layouts stay within the safe area. */
	:global(.app-root) {
		padding-top: env(safe-area-inset-top);
		padding-bottom: env(safe-area-inset-bottom);
		padding-left: env(safe-area-inset-left);
		padding-right: env(safe-area-inset-right);
	}
</style>
