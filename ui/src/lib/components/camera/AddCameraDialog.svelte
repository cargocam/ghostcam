<script lang="ts">
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
	} from '$lib/components/ui/dialog/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { generateEnrollmentQr, fetchPiImages } from '$lib/signaling.js';
	import type { PiImage } from '$lib/api-types';
	import QRCode from 'qrcode';
	import { Download, ChevronDown, ChevronUp } from 'lucide-svelte';

	let {
		open = $bindable(false),
	}: {
		open?: boolean;
	} = $props();

	let wifiSsid = $state('');
	let wifiPassword = $state('');
	let ttlHours = $state(24);
	let qrSvg = $state('');
	let provisionToken = $state('');
	let loading = $state(false);
	let error = $state('');

	let piImages = $state<PiImage[]>([]);
	let imagesExpanded = $state(false);

	// Reset state when dialog closes, fetch images when it opens
	$effect(() => {
		if (!open) {
			qrSvg = '';
			provisionToken = '';
			error = '';
			wifiSsid = '';
			wifiPassword = '';
			ttlHours = 24;
			imagesExpanded = false;
		} else {
			fetchPiImages().then((r) => { piImages = r.images ?? []; }).catch(() => {});
		}
	});

	async function generateQr() {
		loading = true;
		error = '';
		try {
			const resp = await generateEnrollmentQr({
				wifi_ssid: wifiSsid || undefined,
				wifi_password: wifiPassword || undefined,
				ttl_hours: ttlHours,
			});
			provisionToken = resp.token;
			qrSvg = await QRCode.toString(resp.payload, { type: 'svg', margin: 1, width: 256 });
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to generate QR code';
		} finally {
			loading = false;
		}
	}
</script>

<Dialog bind:open>
	<DialogContent>
		<DialogHeader>
			<DialogTitle>Add Camera</DialogTitle>
			<DialogDescription>
				Generate a QR code to enroll cameras. Any unclaimed camera can scan it.
			</DialogDescription>
		</DialogHeader>

		<div class="mt-4 space-y-4">
			{#if piImages.length > 0}
				<button
					type="button"
					class="w-full flex items-center justify-between text-sm text-muted-foreground hover:text-foreground transition-colors py-1"
					onclick={() => { imagesExpanded = !imagesExpanded; }}
				>
					<span>Need to flash a Pi?</span>
					{#if imagesExpanded}
						<ChevronUp class="h-4 w-4" />
					{:else}
						<ChevronDown class="h-4 w-4" />
					{/if}
				</button>
				{#if imagesExpanded}
					<div class="grid gap-2">
						{#each piImages as img}
							<a
								href={img.download_url}
								class="flex items-center justify-between rounded-md border border-border px-3 py-2 text-sm hover:bg-accent transition-colors"
								download
							>
								<div>
									<span class="font-medium">{img.device === 'zero2w' ? 'Pi Zero 2 W' : img.device === 'pi4' ? 'Pi 4' : 'Pi 5'}</span>
									<span class="text-muted-foreground ml-2">{(img.size_bytes / (1024 * 1024)).toFixed(0)} MB</span>
								</div>
								<Download class="h-4 w-4 text-muted-foreground" />
							</a>
						{/each}
						<p class="text-xs text-muted-foreground">{piImages[0].version} · Flash with <a href="https://www.raspberrypi.com/software/" target="_blank" rel="noopener" class="underline">Raspberry Pi Imager</a></p>
					</div>
				{/if}
			{/if}

			{#if !qrSvg}
				<!-- Form -->
				<div class="space-y-3">
					<div>
						<label for="wifi-ssid" class="text-sm font-medium">WiFi Network (optional)</label>
						<input
							id="wifi-ssid"
							type="text"
							bind:value={wifiSsid}
							placeholder="SSID"
							class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
						/>
					</div>

					{#if wifiSsid}
						<div>
							<label for="wifi-password" class="text-sm font-medium">WiFi Password</label>
							<input
								id="wifi-password"
								type="password"
								bind:value={wifiPassword}
								placeholder="Password"
								class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
							/>
						</div>
					{/if}

					<div>
						<label for="ttl" class="text-sm font-medium">Token Expiry</label>
						<select
							id="ttl"
							bind:value={ttlHours}
							class="mt-1 w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
						>
							<option value={1}>1 hour</option>
							<option value={24}>24 hours</option>
							<option value={168}>7 days</option>
						</select>
					</div>
				</div>

				{#if error}
					<p class="text-sm text-destructive">{error}</p>
				{/if}

				<Button onclick={generateQr} disabled={loading} class="w-full">
					{loading ? 'Generating...' : 'Generate QR Code'}
				</Button>
			{:else}
				<!-- QR code display -->
				<div class="flex flex-col items-center gap-4">
					<div class="bg-white p-4 rounded-lg">
						{@html qrSvg}
					</div>

					<div class="text-center space-y-1">
						<p class="text-sm text-muted-foreground">
							Scan this QR code with a camera, or provision via CLI:
						</p>
						<code class="block text-xs bg-muted px-2 py-1 rounded select-all break-all">
							--provision-token {provisionToken}
						</code>
						{#if wifiSsid}
							<p class="text-xs text-muted-foreground">
								Includes WiFi credentials for <span class="font-medium">{wifiSsid}</span>.
							</p>
						{/if}
					</div>

					<Button variant="outline" onclick={() => (qrSvg = '')} class="w-full">
						Generate New Code
					</Button>
				</div>
			{/if}
		</div>
	</DialogContent>
</Dialog>
