<script lang="ts">
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
	} from '$lib/components/ui/dialog/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { Plus, Trash2, AlertTriangle, Info } from 'lucide-svelte';

	/**
	 * Battery-rule shape — must match the Go `BatteryRule` /
	 * Python `ghostcam.power_mode.BatteryRule` exactly.
	 *
	 * Camera evaluates these in lowest-threshold-first order: when
	 * battery_pct falls AT OR BELOW the threshold, that rule fires.
	 * If multiple rules apply (battery below several thresholds), the
	 * lowest threshold wins (most aggressive applicable rule). When
	 * battery climbs back above the threshold, the rule stops firing
	 * and the manually-set or schedule-driven mode takes over.
	 */
	export interface BatteryRule {
		threshold_pct: number;
		power_mode: 'live' | 'standby' | 'sleep';
		upload_mode: 'proactive' | 'lazy';
	}

	let {
		open = $bindable(false),
		initial = '',
		batteryPctReporting = false,
		onsave,
		onclose,
	}: {
		open?: boolean;
		initial?: string;
		/** Whether THIS camera is actively reporting battery_pct. Disables
		 * the editor body with an explanatory note when false — the rules
		 * are valid but inert without a HAT (see GH issue #73). */
		batteryPctReporting?: boolean;
		onsave: (json: string) => void | Promise<void>;
		onclose?: () => void;
	} = $props();

	let rules = $state<BatteryRule[]>([]);
	let saving = $state(false);
	let error = $state('');

	function parseInitial(): BatteryRule[] {
		if (!initial) return [];
		try {
			const parsed = JSON.parse(initial);
			if (!Array.isArray(parsed)) return [];
			return parsed.map((r) => ({
				threshold_pct: Number(r.threshold_pct ?? 20),
				power_mode: (r.power_mode ?? 'sleep') as BatteryRule['power_mode'],
				upload_mode: (r.upload_mode ?? 'lazy') as BatteryRule['upload_mode'],
			}));
		} catch {
			return [];
		}
	}

	$effect(() => {
		if (open) {
			rules = parseInitial();
			error = '';
		} else {
			onclose?.();
		}
	});

	function addRule() {
		const lowestExisting = rules.length === 0
			? 20
			: Math.min(...rules.map((r) => r.threshold_pct));
		rules = [
			...rules,
			{
				threshold_pct: Math.max(0, lowestExisting - 10),
				power_mode: 'sleep',
				upload_mode: 'lazy',
			},
		];
	}

	function removeRule(i: number) {
		rules = rules.filter((_, idx) => idx !== i);
	}

	function validate(): string | null {
		const seen = new Set<number>();
		for (let i = 0; i < rules.length; i++) {
			const r = rules[i];
			if (!Number.isFinite(r.threshold_pct) || r.threshold_pct < 0 || r.threshold_pct > 100) {
				return `Rule ${i + 1}: threshold must be 0–100`;
			}
			if (seen.has(r.threshold_pct)) {
				return `Rule ${i + 1}: duplicate threshold ${r.threshold_pct}%`;
			}
			seen.add(r.threshold_pct);
		}
		return null;
	}

	async function save() {
		const err = validate();
		if (err) {
			error = err;
			return;
		}
		saving = true;
		try {
			// Sort ascending so the wire matches what the camera expects
			// (camera applies rules in lowest-threshold-first priority).
			const sorted = [...rules].sort((a, b) => a.threshold_pct - b.threshold_pct);
			await onsave(JSON.stringify(sorted));
			open = false;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to save battery rules';
		} finally {
			saving = false;
		}
	}
</script>

<Dialog bind:open>
	<DialogContent class="max-w-2xl">
		<DialogHeader>
			<DialogTitle>Battery rules</DialogTitle>
			<DialogDescription>
				Force the camera into low-power modes when battery state-of-charge
				drops below a threshold. Rules fire at the lowest applicable
				threshold (most aggressive wins) and stop firing automatically
				when the battery climbs back above the threshold.
			</DialogDescription>
		</DialogHeader>

		{#if !batteryPctReporting}
			<div class="mt-3 flex items-start gap-2 rounded-md border border-blue-500/40 bg-blue-500/10 p-3 text-xs text-blue-700 dark:text-blue-400">
				<Info class="h-3.5 w-3.5 shrink-0 mt-0.5" />
				<div>
					<p class="font-medium">No battery sensor detected</p>
					<p class="mt-0.5">
						This camera isn't currently reporting <code class="font-mono">battery_pct</code>
						telemetry, so these rules are inert. Rules are still saved and
						become active automatically once a battery-sensing HAT (PiSugar
						or generic UPS) is wired up — see GitHub issue #73.
					</p>
				</div>
			</div>
		{/if}

		<div class="mt-4 space-y-3 max-h-[60vh] overflow-y-auto">
			{#each rules as rule, i (i)}
				<div class="rounded-lg border border-input p-3 space-y-2">
					<div class="flex items-center gap-2">
						<span class="text-xs font-medium text-muted-foreground w-8">#{i + 1}</span>
						<span class="text-sm">When battery ≤</span>
						<input
							type="number"
							min="0"
							max="100"
							step="1"
							bind:value={rule.threshold_pct}
							class="w-16 rounded-md border border-input bg-background px-2 py-1 text-sm font-mono"
						/>
						<span class="text-sm text-muted-foreground">%</span>
						<button
							type="button"
							class="ml-auto rounded p-1 text-muted-foreground hover:text-destructive hover:bg-destructive/10"
							onclick={() => removeRule(i)}
							aria-label="Remove rule"
						>
							<Trash2 class="h-4 w-4" />
						</button>
					</div>

					<div class="grid grid-cols-2 gap-2">
						<select
							bind:value={rule.power_mode}
							class="rounded-md border border-input bg-background px-2 py-1 text-sm"
						>
							<option value="live">Live</option>
							<option value="standby">Standby</option>
							<option value="sleep">Sleep</option>
						</select>
						<select
							bind:value={rule.upload_mode}
							class="rounded-md border border-input bg-background px-2 py-1 text-sm"
						>
							<option value="proactive">Proactive upload</option>
							<option value="lazy">Lazy upload</option>
						</select>
					</div>
				</div>
			{/each}

			{#if rules.length === 0}
				<div class="rounded-lg border border-dashed border-input p-6 text-center text-sm text-muted-foreground">
					No battery rules. Add one to automatically conserve power when
					the battery gets low.
				</div>
			{/if}

			<Button variant="outline" class="w-full" onclick={addRule}>
				<Plus class="h-4 w-4 mr-1" />
				Add rule
			</Button>
		</div>

		{#if error}
			<div class="mt-3 flex items-start gap-1.5 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
				<AlertTriangle class="h-3.5 w-3.5 shrink-0 mt-0.5" />
				<span>{error}</span>
			</div>
		{/if}

		<div class="mt-4 flex gap-2">
			<Button variant="outline" class="flex-1" disabled={saving} onclick={() => (open = false)}>
				Cancel
			</Button>
			<Button class="flex-1" disabled={saving} onclick={save}>
				{saving ? 'Saving…' : `Save ${rules.length} rule${rules.length === 1 ? '' : 's'}`}
			</Button>
		</div>
	</DialogContent>
</Dialog>
