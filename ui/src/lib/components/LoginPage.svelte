<script lang="ts">
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { authStore } from '$lib/stores/auth.svelte.js';
	import { requestLoginOTP, verifyLoginOTP, login as authLogin } from '$lib/auth.js';
	import { Button } from '$lib/components/ui/button/index.js';

	const PREF_KEY = 'ghostcam-login-method';

	let email = $state('');
	let password = $state('');
	let otpCode = $state('');
	let error = $state('');
	let loading = $state(false);

	// OTP flow states
	let useOTP = $state(localStorage.getItem(PREF_KEY) === 'otp');
	let otpSent = $state(false);
	let otpSending = $state(false);

	function toggleMethod() {
		useOTP = !useOTP;
		localStorage.setItem(PREF_KEY, useOTP ? 'otp' : 'password');
		error = '';
		otpSent = false;
		otpCode = '';
	}

	async function handlePasswordLogin(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		loading = true;
		try {
			const ok = await transportStore.login(email, password);
			if (!ok) {
				error = 'Invalid email or password';
			}
		} catch {
			error = 'Connection failed';
		} finally {
			loading = false;
		}
	}

	async function handleSendOTP() {
		error = '';
		otpSending = true;
		try {
			await requestLoginOTP(email);
			otpSent = true;
		} catch {
			error = 'Failed to send code';
		} finally {
			otpSending = false;
		}
	}

	async function handleVerifyOTP(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		loading = true;
		try {
			const ok = await verifyLoginOTP(email, otpCode);
			if (ok) {
				authStore.refresh();
				transportStore.authenticated = true;
				await transportStore.initialize();
			} else {
				error = 'Invalid or expired code';
			}
		} catch {
			error = 'Connection failed';
		} finally {
			loading = false;
		}
	}
</script>

<div class="flex h-screen-stable items-center justify-center bg-background">
	<div class="w-full max-w-sm space-y-6 px-4">
		<div class="text-center space-y-2">
			<h1 class="text-2xl font-bold tracking-tight">Ghostcam</h1>
			<p class="text-sm text-muted-foreground">Sign in to continue</p>
		</div>

		{#if useOTP}
			<!-- OTP login flow -->
			{#if !otpSent}
				<!-- Step 1: enter email and send code -->
				<div class="space-y-4">
					<div>
						<input
							type="email"
							name="email"
							autocomplete="email"
							bind:value={email}
							placeholder="Email"
							required
							class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
							autofocus
						/>
					</div>

					{#if error}
						<p class="text-sm text-destructive">{error}</p>
					{/if}

					<Button class="w-full" disabled={otpSending || !email} onclick={handleSendOTP}>
						{otpSending ? 'Sending...' : 'Send login code'}
					</Button>
				</div>
			{:else}
				<!-- Step 2: enter code -->
				<form onsubmit={handleVerifyOTP} class="space-y-4">
					<p class="text-sm text-muted-foreground text-center">
						We sent a 6-digit code to <strong>{email}</strong>
					</p>
					<div>
						<input
							type="text"
							inputmode="numeric"
							pattern="[0-9]*"
							maxlength="6"
							autocomplete="one-time-code"
							bind:value={otpCode}
							placeholder="000000"
							required
							class="w-full rounded-md border bg-transparent px-3 py-2 text-center text-lg tracking-widest font-mono ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
							autofocus
						/>
					</div>

					{#if error}
						<p class="text-sm text-destructive">{error}</p>
					{/if}

					<Button type="submit" class="w-full" disabled={loading || otpCode.length < 6}>
						{loading ? 'Verifying...' : 'Verify code'}
					</Button>

					<div class="text-center">
						<button
							type="button"
							class="text-sm text-muted-foreground hover:underline"
							onclick={() => { otpSent = false; otpCode = ''; error = ''; }}
						>
							Resend code
						</button>
					</div>
				</form>
			{/if}
		{:else}
			<!-- Password login form -->
			<form onsubmit={handlePasswordLogin} class="space-y-4">
				<div>
					<input
						type="email"
						name="email"
						autocomplete="email"
						bind:value={email}
						placeholder="Email"
						required
						class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						autofocus
					/>
				</div>

				<div>
					<input
						type="password"
						name="password"
						autocomplete="current-password"
						bind:value={password}
						placeholder="Password"
						required
						class="w-full rounded-md border bg-transparent px-3 py-2 text-sm ring-offset-background placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					/>
				</div>

				{#if error}
					<p class="text-sm text-destructive">{error}</p>
				{/if}

				<Button type="submit" class="w-full" disabled={loading || !email || !password}>
					{loading ? 'Signing in...' : 'Sign in'}
				</Button>

				<div class="text-center">
					<a href="/reset-password" class="text-sm text-muted-foreground hover:underline">
						Forgot password?
					</a>
				</div>
			</form>
		{/if}

		<!-- Toggle between password and OTP -->
		<div class="text-center">
			<button
				type="button"
				class="text-sm text-muted-foreground hover:underline"
				onclick={toggleMethod}
			>
				{useOTP ? 'Sign in with password instead' : 'Sign in with email code instead'}
			</button>
		</div>
	</div>
</div>
