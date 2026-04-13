<script lang="ts">
	import { Button, buttonVariants } from '$lib/components/ui/button/index.js';
	import { cameraStore } from '$lib/stores/cameras.svelte.js';
	import { fetchPiImages, generateEnrollmentQr } from '$lib/signaling.js';
	import type { PiImage } from '$lib/api-types';
	import QRCode from 'qrcode';
	import {
		Check,
		ChevronDown,
		ChevronRight,
		Cpu,
		Download,
		Loader2,
		QrCode,
	} from 'lucide-svelte';

	type Step = 'pick' | 'flash' | 'connect' | 'waiting';
	type Device = 'zero2w' | 'pi4' | 'pi5';

	const STEP_KEY = 'ghostcam-onboarding-step';
	const DEVICE_KEY = 'ghostcam-onboarding-device';
	const DISMISS_KEY = 'ghostcam-onboarding-dismissed';

	const DEVICES: { id: Device; name: string; resolution: string; tagline: string }[] = [
		{ id: 'zero2w', name: 'Raspberry Pi Zero 2 W', resolution: '480p', tagline: 'Cheapest & smallest. Good for simple monitoring.' },
		{ id: 'pi4', name: 'Raspberry Pi 4', resolution: '720p', tagline: 'Balanced. Handles HD recording and multiple streams.' },
		{ id: 'pi5', name: 'Raspberry Pi 5', resolution: '1080p', tagline: 'Fastest. Full HD with headroom for heavy workloads.' },
	];

	let {
		onDismiss,
	}: {
		onDismiss?: () => void;
	} = $props();

	let step = $state<Step>(readStep());
	let selectedDevice = $state<Device | null>(readDevice());
	let flashInstructionsOpen = $state(false);

	let images = $state<PiImage[]>([]);
	let imagesLoading = $state(false);
	let imagesError = $state('');

	let wifiSsid = $state('');
	let wifiPassword = $state('');
	let qrSvg = $state('');
	let provisionToken = $state('');
	let qrLoading = $state(false);
	let qrError = $state('');

	const initialCameraIds = new Set(cameraStore.cameras.map((c) => c.device_id));
	let cameraConnected = $state(false);

	// Persist step / device selection so a mid-flow refresh resumes in place.
	$effect(() => {
		try {
			localStorage.setItem(STEP_KEY, step);
		} catch {
			/* quota / SSR / private mode — not fatal */
		}
	});
	$effect(() => {
		try {
			if (selectedDevice) localStorage.setItem(DEVICE_KEY, selectedDevice);
			else localStorage.removeItem(DEVICE_KEY);
		} catch {
			/* ignore */
		}
	});

	// Pull the image list on mount so step 2 can render immediately.
	$effect(() => {
		loadImages();
	});

	// Watch for a new camera to appear once we're in the "waiting" step.
	// Any device_id not in the snapshot we took on mount counts as "the
	// camera the user just connected" — even if it races ahead of the
	// explicit "waiting" step.
	$effect(() => {
		const fresh = cameraStore.cameras.find((c) => !initialCameraIds.has(c.device_id));
		if (fresh) {
			cameraConnected = true;
			// Auto-dismiss after a short celebration window.
			const t = setTimeout(() => {
				dismiss();
			}, 3000);
			return () => clearTimeout(t);
		}
	});

	async function loadImages() {
		imagesLoading = true;
		imagesError = '';
		try {
			const resp = await fetchPiImages();
			images = resp.images ?? [];
		} catch (e) {
			imagesError = e instanceof Error ? e.message : 'Failed to load images';
		} finally {
			imagesLoading = false;
		}
	}

	function imageForDevice(device: Device): PiImage | undefined {
		return images.find((img) => img.device === device);
	}

	function formatBytes(bytes: number): string {
		if (!bytes) return '';
		const gb = bytes / (1024 * 1024 * 1024);
		if (gb >= 1) return `${gb.toFixed(1)} GB`;
		const mb = bytes / (1024 * 1024);
		return `${mb.toFixed(0)} MB`;
	}

	function pickDevice(device: Device) {
		selectedDevice = device;
		step = 'flash';
		flashInstructionsOpen = false;
	}

	function flashDone() {
		step = 'connect';
	}

	async function generateQr() {
		qrLoading = true;
		qrError = '';
		try {
			const resp = await generateEnrollmentQr({
				wifi_ssid: wifiSsid || undefined,
				wifi_password: wifiPassword || undefined,
				ttl_hours: 24,
			});
			provisionToken = resp.token;
			qrSvg = await QRCode.toString(resp.payload, { type: 'svg', margin: 1, width: 256 });
			step = 'waiting';
		} catch (e) {
			qrError = e instanceof Error ? e.message : 'Failed to generate QR code';
		} finally {
			qrLoading = false;
		}
	}

	function dismiss() {
		try {
			localStorage.setItem(DISMISS_KEY, '1');
			localStorage.removeItem(STEP_KEY);
			localStorage.removeItem(DEVICE_KEY);
		} catch {
			/* ignore */
		}
		onDismiss?.();
	}

	function readStep(): Step {
		try {
			const raw = localStorage.getItem(STEP_KEY);
			if (raw === 'pick' || raw === 'flash' || raw === 'connect' || raw === 'waiting') return raw;
		} catch {
			/* ignore */
		}
		return 'pick';
	}

	function readDevice(): Device | null {
		try {
			const raw = localStorage.getItem(DEVICE_KEY);
			if (raw === 'zero2w' || raw === 'pi4' || raw === 'pi5') return raw;
		} catch {
			/* ignore */
		}
		return null;
	}

	let selectedImage = $derived(selectedDevice ? imageForDevice(selectedDevice) : undefined);
</script>

<div class="mx-auto max-w-2xl space-y-4 py-8 px-4">
	<div class="text-center space-y-1">
		<h2 class="text-2xl font-semibold">Get started</h2>
		<p class="text-sm text-muted-foreground">
			Four steps to stream from a Raspberry Pi camera, no terminal required.
		</p>
	</div>

	<!-- Step 1: Pick your Pi -->
	<section
		class="rounded-lg border bg-card p-5 space-y-3"
		class:opacity-60={step !== 'pick' && selectedDevice}
	>
		<header class="flex items-center gap-2">
			{@render StepBadge(1, selectedDevice !== null, step === 'pick')}
			<h3 class="font-medium">Pick your Pi</h3>
			{#if selectedDevice && step !== 'pick'}
				<button class="ml-auto text-xs text-muted-foreground hover:underline" onclick={() => (step = 'pick')}>
					Change
				</button>
			{/if}
		</header>

		{#if step === 'pick' || !selectedDevice}
			<div class="grid gap-2 sm:grid-cols-3">
				{#each DEVICES as dev (dev.id)}
					<button
						class="flex flex-col items-start gap-1 rounded-md border bg-background p-3 text-left transition hover:border-primary hover:bg-accent/40 focus:outline-none focus:ring-2 focus:ring-primary"
						class:border-primary={selectedDevice === dev.id}
						onclick={() => pickDevice(dev.id)}
					>
						<div class="flex w-full items-center gap-1.5">
							<Cpu class="h-4 w-4 text-muted-foreground" />
							<span class="text-sm font-medium">{dev.name}</span>
						</div>
						<span class="text-xs font-mono text-primary">{dev.resolution}</span>
						<span class="text-xs text-muted-foreground">{dev.tagline}</span>
					</button>
				{/each}
			</div>
		{:else}
			<p class="text-sm text-muted-foreground">
				{DEVICES.find((d) => d.id === selectedDevice)?.name} &middot;
				{DEVICES.find((d) => d.id === selectedDevice)?.resolution}
			</p>
		{/if}
	</section>

	<!-- Step 2: Download & Flash -->
	{#if selectedDevice}
		<section
			class="rounded-lg border bg-card p-5 space-y-3"
			class:opacity-60={step !== 'flash' && step !== 'pick' && step !== 'connect' ? false : step !== 'flash' && step !== 'pick'}
		>
			<header class="flex items-center gap-2">
				{@render StepBadge(2, step === 'connect' || step === 'waiting', step === 'flash')}
				<h3 class="font-medium">Download &amp; flash SD card</h3>
			</header>

			{#if step === 'flash' || step === 'pick'}
				{#if imagesLoading}
					<p class="flex items-center gap-2 text-sm text-muted-foreground">
						<Loader2 class="h-4 w-4 animate-spin" /> Loading available images…
					</p>
				{:else if imagesError}
					<p class="text-sm text-destructive">Could not load images: {imagesError}</p>
					<Button variant="outline" size="sm" onclick={loadImages}>Retry</Button>
				{:else if selectedImage}
					<div class="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
						<div class="space-y-0.5">
							<p class="text-sm">
								<span class="font-mono">ghostcam-{selectedDevice}-{selectedImage.version}.img.xz</span>
							</p>
							<p class="text-xs text-muted-foreground">
								{formatBytes(selectedImage.size_bytes)} &middot;
								SHA-256 <code class="font-mono">{selectedImage.sha256.slice(0, 12)}…</code>
							</p>
						</div>
						<a
							href={selectedImage.download_url}
							target="_blank"
							rel="noopener noreferrer"
							class={buttonVariants()}
						>
							<Download class="h-4 w-4 mr-1.5" /> Download image
						</a>
					</div>
				{:else}
					<div class="rounded-md bg-muted p-3 text-sm text-muted-foreground">
						No pre-built image is available for this Pi yet. See the
						<a href="/docs/usage" class="underline">documentation</a>
						for manual setup instructions.
					</div>
				{/if}

				<div>
					<button
						type="button"
						class="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
						onclick={() => (flashInstructionsOpen = !flashInstructionsOpen)}
					>
						{#if flashInstructionsOpen}
							<ChevronDown class="h-3.5 w-3.5" />
						{:else}
							<ChevronRight class="h-3.5 w-3.5" />
						{/if}
						Flashing instructions
					</button>
					{#if flashInstructionsOpen}
						<div class="mt-2 space-y-3 rounded-md bg-muted/50 p-3 text-xs">
							<div>
								<p class="font-medium mb-1">Raspberry Pi Imager (easiest)</p>
								<ol class="list-decimal pl-4 space-y-0.5 text-muted-foreground">
									<li>Open Raspberry Pi Imager.</li>
									<li>Choose OS → "Use custom" → select the downloaded <code>.img.xz</code>.</li>
									<li>Choose your SD card and flash.</li>
								</ol>
							</div>
							<div>
								<p class="font-medium mb-1">macOS / Linux (<code>dd</code>)</p>
								<pre class="overflow-x-auto rounded bg-background p-2 font-mono">xz -dk ghostcam-{selectedDevice}-{selectedImage?.version ?? 'vX.Y.Z'}.img.xz
sudo dd if=ghostcam-{selectedDevice}-{selectedImage?.version ?? 'vX.Y.Z'}.img of=/dev/rdiskN bs=4M status=progress
sudo sync</pre>
								<p class="text-muted-foreground mt-1">
									Replace <code>/dev/rdiskN</code> (macOS) or <code>/dev/sdX</code> (Linux) with your SD card device.
								</p>
							</div>
						</div>
					{/if}
				</div>

				<Button onclick={flashDone} class="w-full sm:w-auto">
					I've flashed the SD card
				</Button>
			{/if}
		</section>
	{/if}

	<!-- Step 3: Connect your camera -->
	{#if step === 'connect' || step === 'waiting'}
		<section class="rounded-lg border bg-card p-5 space-y-3" class:opacity-60={step !== 'connect'}>
			<header class="flex items-center gap-2">
				{@render StepBadge(3, step === 'waiting', step === 'connect')}
				<h3 class="font-medium">Connect your camera</h3>
			</header>

			{#if step === 'connect'}
				<p class="text-sm text-muted-foreground">
					Boot the Pi with the flashed SD card inserted, then scan this QR code with the Pi camera
					to enroll it on your account.
				</p>

				<div class="space-y-2">
					<div>
						<label for="onboarding-wifi" class="text-xs font-medium">WiFi network (optional)</label>
						<input
							id="onboarding-wifi"
							type="text"
							bind:value={wifiSsid}
							placeholder="SSID"
							class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
						/>
					</div>
					{#if wifiSsid}
						<div>
							<label for="onboarding-wifi-pass" class="text-xs font-medium">WiFi password</label>
							<input
								id="onboarding-wifi-pass"
								type="password"
								bind:value={wifiPassword}
								placeholder="Password"
								class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
							/>
						</div>
					{/if}
				</div>

				{#if qrError}
					<p class="text-sm text-destructive">{qrError}</p>
				{/if}

				<Button onclick={generateQr} disabled={qrLoading} class="w-full sm:w-auto">
					<QrCode class="h-4 w-4 mr-1.5" />
					{qrLoading ? 'Generating…' : 'Generate QR code'}
				</Button>
			{/if}
		</section>
	{/if}

	<!-- Step 4: Waiting -->
	{#if step === 'waiting'}
		<section class="rounded-lg border bg-card p-5 space-y-3">
			<header class="flex items-center gap-2">
				{@render StepBadge(4, cameraConnected, !cameraConnected)}
				<h3 class="font-medium">
					{cameraConnected ? 'Camera connected!' : 'Waiting for your camera…'}
				</h3>
			</header>

			{#if !cameraConnected && qrSvg}
				<div class="flex flex-col items-center gap-4 sm:flex-row sm:items-start">
					<div class="rounded-lg bg-white p-3 shrink-0">
						{@html qrSvg}
					</div>
					<div class="flex-1 space-y-2 text-sm">
						<p>
							Hold this QR code in front of the Pi camera after it boots. The camera has a
							5-minute window to scan — reboot it if you miss the window.
						</p>
						<p class="text-xs text-muted-foreground">
							Or provision via CLI with
							<code class="block mt-1 rounded bg-muted px-2 py-1 font-mono text-xs break-all">
								--provision-token {provisionToken}
							</code>
						</p>
						<p class="flex items-center gap-1.5 text-xs text-muted-foreground">
							<Loader2 class="h-3.5 w-3.5 animate-spin" /> Watching for a new camera…
						</p>
					</div>
				</div>
			{:else if cameraConnected}
				<div class="flex flex-col items-center gap-2 py-4">
					<div class="flex h-12 w-12 items-center justify-center rounded-full bg-primary/10 text-primary">
						<Check class="h-6 w-6" />
					</div>
					<p class="text-sm text-muted-foreground">
						All set — this guide will close in a moment.
					</p>
				</div>
			{/if}
		</section>
	{/if}

	<div class="text-center pt-2">
		<button
			class="text-xs text-muted-foreground hover:underline"
			onclick={dismiss}
		>
			Skip setup guide
		</button>
	</div>
</div>

{#snippet StepBadge(number: number, complete: boolean, active: boolean)}
	{#if complete}
		<span class="flex h-6 w-6 items-center justify-center rounded-full bg-primary text-primary-foreground">
			<Check class="h-3.5 w-3.5" />
		</span>
	{:else}
		<span
			class="flex h-6 w-6 items-center justify-center rounded-full text-xs font-semibold"
			class:bg-primary={active}
			class:text-primary-foreground={active}
			class:bg-muted={!active}
			class:text-muted-foreground={!active}
		>
			{number}
		</span>
	{/if}
{/snippet}
