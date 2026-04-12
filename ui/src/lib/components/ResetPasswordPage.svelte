<script lang="ts">
	import { Button } from '$lib/components/ui/button/index.js';
	import { forgotPassword, resetPassword } from '$lib/auth.js';

	// Determine mode from URL: if ?token= present, show reset form; otherwise show forgot form.
	let token = $state(new URLSearchParams(window.location.search).get('token') ?? '');

	// Forgot password state
	let forgotEmail = $state('');
	let forgotSubmitting = $state(false);
	let forgotSent = $state(false);

	async function handleForgot(e: SubmitEvent) {
		e.preventDefault();
		forgotSubmitting = true;
		await forgotPassword(forgotEmail);
		forgotSubmitting = false;
		forgotSent = true;
	}

	// Reset password state
	let newPassword = $state('');
	let confirmPassword = $state('');
	let resetError = $state('');
	let resetSubmitting = $state(false);
	let resetSuccess = $state(false);

	async function handleReset(e: SubmitEvent) {
		e.preventDefault();
		resetError = '';
		if (newPassword !== confirmPassword) {
			resetError = 'Passwords do not match';
			return;
		}
		if (newPassword.length < 8 || newPassword.length > 128) {
			resetError = 'Password must be 8-128 characters';
			return;
		}
		resetSubmitting = true;
		const result = await resetPassword(token, newPassword);
		resetSubmitting = false;
		if (!result.ok) {
			resetError = result.error ?? 'Reset failed';
			return;
		}
		resetSuccess = true;
	}
</script>

<div class="flex h-screen-stable items-center justify-center bg-background">
	<div class="w-full max-w-sm px-6">
		<div class="flex flex-col items-center gap-4 mb-8">
			<img src="/icon.svg" alt="" class="h-12 w-12" />
			<h1 class="text-2xl font-bold tracking-tight">Ghostcam</h1>
		</div>

		{#if token}
			<!-- Reset form -->
			{#if resetSuccess}
				<div class="text-center space-y-4">
					<p class="text-sm">Your password has been reset.</p>
					<a href="/" class="text-sm text-primary hover:underline">Log in</a>
				</div>
			{:else}
				<h2 class="text-lg font-semibold mb-4 text-center">Set a new password</h2>
				<form onsubmit={handleReset} class="space-y-4">
					<div>
						<label for="rp-new" class="text-xs text-muted-foreground mb-1 block">New password</label>
						<input
							id="rp-new"
							type="password"
							autocomplete="new-password"
							bind:value={newPassword}
							required
							minlength="8"
							maxlength="128"
							class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						/>
					</div>
					<div>
						<label for="rp-confirm" class="text-xs text-muted-foreground mb-1 block">Confirm password</label>
						<input
							id="rp-confirm"
							type="password"
							autocomplete="new-password"
							bind:value={confirmPassword}
							required
							minlength="8"
							maxlength="128"
							class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						/>
					</div>
					{#if resetError}
						<p class="text-sm text-destructive">{resetError}</p>
					{/if}
					<Button type="submit" class="w-full" disabled={resetSubmitting || !newPassword || !confirmPassword}>
						{resetSubmitting ? 'Resetting...' : 'Reset password'}
					</Button>
				</form>
			{/if}
		{:else}
			<!-- Forgot password form -->
			{#if forgotSent}
				<div class="text-center space-y-4">
					<p class="text-sm">If an account exists with that email, you'll receive a reset link shortly.</p>
					<a href="/" class="text-sm text-primary hover:underline">Back to login</a>
				</div>
			{:else}
				<h2 class="text-lg font-semibold mb-4 text-center">Forgot password</h2>
				<form onsubmit={handleForgot} class="space-y-4">
					<div>
						<label for="fp-email" class="text-xs text-muted-foreground mb-1 block">Email</label>
						<input
							id="fp-email"
							type="email"
							autocomplete="email"
							bind:value={forgotEmail}
							required
							class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						/>
					</div>
					<Button type="submit" class="w-full" disabled={forgotSubmitting || !forgotEmail}>
						{forgotSubmitting ? 'Sending...' : 'Send reset link'}
					</Button>
					<div class="text-center">
						<a href="/" class="text-sm text-muted-foreground hover:underline">Back to login</a>
					</div>
				</form>
			{/if}
		{/if}
	</div>
</div>
