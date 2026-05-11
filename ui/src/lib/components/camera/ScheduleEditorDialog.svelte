<script lang="ts">
	import {
		Dialog,
		DialogContent,
		DialogHeader,
		DialogTitle,
		DialogDescription,
	} from '$lib/components/ui/dialog/index.js';
	import { Button } from '$lib/components/ui/button/index.js';
	import { Plus, Trash2, AlertTriangle } from 'lucide-svelte';

	/**
	 * Schedule rule shape — must match the Go `ScheduleWindow` /
	 * Python `ghostcam.power_mode.ScheduleWindow` exactly. Keep the
	 * field names + types in lockstep with both.
	 */
	export interface ScheduleRule {
		start: string; // "HH:MM"
		end: string;   // "HH:MM"
		power_mode: 'live' | 'standby' | 'sleep';
		upload_mode: 'proactive' | 'lazy';
		days?: number[]; // 0=Mon … 6=Sun. Empty/absent = all days.
	}

	let {
		open = $bindable(false),
		initial = '',
		onsave,
		onclose,
	}: {
		open?: boolean;
		initial?: string;
		onsave: (json: string) => void | Promise<void>;
		onclose?: () => void;
	} = $props();

	let rules = $state<ScheduleRule[]>([]);
	let saving = $state(false);
	let error = $state('');

	const DAY_LABELS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

	function parseInitial(): ScheduleRule[] {
		if (!initial) return [];
		try {
			const parsed = JSON.parse(initial);
			if (!Array.isArray(parsed)) return [];
			return parsed.map((r) => ({
				start: String(r.start ?? '00:00'),
				end: String(r.end ?? '00:00'),
				power_mode: (r.power_mode ?? 'live') as ScheduleRule['power_mode'],
				upload_mode: (r.upload_mode ?? 'proactive') as ScheduleRule['upload_mode'],
				days: Array.isArray(r.days) ? r.days.map(Number) : undefined,
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
		rules = [
			...rules,
			{ start: '22:00', end: '06:00', power_mode: 'sleep', upload_mode: 'lazy' },
		];
	}

	function removeRule(i: number) {
		rules = rules.filter((_, idx) => idx !== i);
	}

	function toggleDay(i: number, day: number) {
		const rule = rules[i];
		const current = rule.days ?? [];
		const next = current.includes(day)
			? current.filter((d) => d !== day)
			: [...current, day].sort((a, b) => a - b);
		// Empty selection means "all days" — we encode it as undefined so
		// the wire never carries an empty array (Go's frozenset() default
		// matches that).
		rules[i] = { ...rule, days: next.length === 0 ? undefined : next };
		rules = [...rules];
	}

	function isValidTime(s: string): boolean {
		const m = /^(\d{2}):(\d{2})$/.exec(s);
		if (!m) return false;
		const h = parseInt(m[1], 10);
		const mm = parseInt(m[2], 10);
		return h >= 0 && h <= 23 && mm >= 0 && mm <= 59;
	}

	function validate(): string | null {
		for (let i = 0; i < rules.length; i++) {
			const r = rules[i];
			if (!isValidTime(r.start)) return `Rule ${i + 1}: invalid start time`;
			if (!isValidTime(r.end)) return `Rule ${i + 1}: invalid end time`;
			if (r.start === r.end) return `Rule ${i + 1}: start and end must differ`;
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
			// Send the array as raw JSON. Empty array clears the schedule
			// on the server side (CameraUpdate.Schedule = empty bytes →
			// SET schedule = NULL).
			await onsave(JSON.stringify(rules));
			open = false;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to save schedule';
		} finally {
			saving = false;
		}
	}
</script>

<Dialog bind:open>
	<DialogContent class="max-w-2xl">
		<DialogHeader>
			<DialogTitle>Schedule</DialogTitle>
			<DialogDescription>
				Switch the camera between power and upload modes based on
				time-of-day. Earlier rules in the list take precedence over later
				ones if their windows overlap. Outside of any active rule the
				camera falls back to the manually-set mode.
			</DialogDescription>
		</DialogHeader>

		<div class="mt-4 space-y-3 max-h-[60vh] overflow-y-auto">
			{#each rules as rule, i (i)}
				<div class="rounded-lg border border-input p-3 space-y-2">
					<div class="flex items-center gap-2">
						<span class="text-xs font-medium text-muted-foreground w-8">#{i + 1}</span>
						<div class="flex items-center gap-1 text-sm">
							<input
								type="text"
								bind:value={rule.start}
								class="w-16 rounded-md border border-input bg-background px-2 py-1 font-mono"
								placeholder="HH:MM"
								maxlength="5"
							/>
							<span class="text-muted-foreground">→</span>
							<input
								type="text"
								bind:value={rule.end}
								class="w-16 rounded-md border border-input bg-background px-2 py-1 font-mono"
								placeholder="HH:MM"
								maxlength="5"
							/>
						</div>
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

					<div class="flex flex-wrap gap-1">
						{#each DAY_LABELS as label, d}
							{@const active = (rule.days ?? [0, 1, 2, 3, 4, 5, 6]).includes(d)}
							<button
								type="button"
								class="rounded-md px-2 py-1 text-xs font-medium border"
								class:bg-primary={active}
								class:text-primary-foreground={active}
								class:border-primary={active}
								class:border-input={!active}
								class:text-muted-foreground={!active}
								onclick={() => toggleDay(i, d)}
							>
								{label}
							</button>
						{/each}
						{#if !rule.days}
							<span class="text-xs text-muted-foreground self-center pl-1">all days</span>
						{/if}
					</div>

					{#if /^(\d{2}):(\d{2})$/.test(rule.start) && /^(\d{2}):(\d{2})$/.test(rule.end) && rule.start > rule.end}
						<p class="text-xs text-muted-foreground">
							Wraps midnight ({rule.start} → 24:00 → {rule.end}).
						</p>
					{/if}
				</div>
			{/each}

			{#if rules.length === 0}
				<div class="rounded-lg border border-dashed border-input p-6 text-center text-sm text-muted-foreground">
					No schedule rules. Add one to override the manual mode during
					specific times of day.
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
