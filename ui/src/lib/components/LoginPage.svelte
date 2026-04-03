<script lang="ts">
	import { transportStore } from '$lib/stores/transport.svelte.js';
	import { Button } from '$lib/components/ui/button/index.js';

	let email = $state('');
	let password = $state('');
	let error = $state('');
	let loading = $state(false);

	async function handleSubmit(e: SubmitEvent) {
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
</script>

<div class="flex h-dvh items-center justify-center bg-background">
	<div class="w-full max-w-sm space-y-6 px-4">
		<div class="text-center space-y-2">
			<h1 class="text-2xl font-bold tracking-tight">Ghostcam</h1>
			<p class="text-sm text-muted-foreground">Sign in to continue</p>
		</div>

		<form onsubmit={handleSubmit} class="space-y-4">
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
				{#if loading}
					Signing in...
				{:else}
					Sign in
				{/if}
			</Button>
		</form>
	</div>
</div>
