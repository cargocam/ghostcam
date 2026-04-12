<script lang="ts">
	import { verifyEmail } from '$lib/auth.js';

	let status = $state<'loading' | 'success' | 'error'>('loading');
	let errorMsg = $state('');

	$effect(() => {
		const params = new URLSearchParams(window.location.search);
		const token = params.get('token');
		if (!token) {
			status = 'error';
			errorMsg = 'No verification token found.';
			return;
		}
		verifyEmail(token).then((result) => {
			if (result.ok) {
				status = 'success';
			} else {
				status = 'error';
				errorMsg = result.error ?? 'Verification failed.';
			}
		});
	});
</script>

<div class="flex h-screen-stable items-center justify-center bg-background">
	<div class="w-full max-w-sm px-6">
		<div class="flex flex-col items-center gap-4 mb-8">
			<img src="/icon.svg" alt="" class="h-12 w-12" />
			<h1 class="text-2xl font-bold tracking-tight">Ghostcam</h1>
		</div>

		{#if status === 'loading'}
			<p class="text-center text-muted-foreground">Verifying your email...</p>
		{:else if status === 'success'}
			<div class="text-center space-y-4">
				<p class="text-sm">Your email has been verified.</p>
				<a href="/" class="text-sm text-primary hover:underline">Go to Ghostcam</a>
			</div>
		{:else}
			<div class="text-center space-y-4">
				<p class="text-sm text-destructive">{errorMsg}</p>
				<a href="/" class="text-sm text-primary hover:underline">Go to Ghostcam</a>
			</div>
		{/if}
	</div>
</div>
