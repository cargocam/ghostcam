<script lang="ts">
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
	} from '$lib/components/ui/dialog/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { generateEnrollmentQr } from '$lib/signaling.js';

	let {
		open = $bindable(false),
	}: {
		open?: boolean;
	} = $props();

	let wifiSsid = $state('');
	let wifiPassword = $state('');
	let ttlHours = $state(24);
	let qrSvg = $state('');
	let loading = $state(false);
	let error = $state('');

	// Reset state when dialog closes
	$effect(() => {
		if (!open) {
			qrSvg = '';
			error = '';
			wifiSsid = '';
			wifiPassword = '';
			ttlHours = 24;
		}
	});

	async function generateQr() {
		loading = true;
		error = '';
		try {
			qrSvg = await generateEnrollmentQr({
				wifi_ssid: wifiSsid || undefined,
				wifi_password: wifiPassword || undefined,
				ttl_hours: ttlHours,
			});
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
							Hold this QR code in front of any unclaimed camera.
						</p>
						<p class="text-xs text-muted-foreground">
							This QR code is reusable -- scan it on multiple cameras.
							{#if wifiSsid}
								Includes WiFi credentials for <span class="font-medium">{wifiSsid}</span>.
							{/if}
						</p>
					</div>

					<Button variant="outline" onclick={() => (qrSvg = '')} class="w-full">
						Generate New Code
					</Button>
				</div>
			{/if}
		</div>
	</DialogContent>
</Dialog>
