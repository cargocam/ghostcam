// Barrel re-export for every type that crosses the wire between the
// Ghostcam UI and the Go server. Import from `$lib/api-types` in both
// application code and browser-test fixtures so a `make generate-types`
// diff is the only thing that can break the contract.
//
// Every symbol here is generated from a Go struct — do NOT add manual
// types to this file. See:
//   - common.ts         from common/
//   - apitypes.ts       from server/apitypes/
//
// Regenerate with `make generate-types`. CI refuses a PR whose checked-in
// files are stale.

export * from './common';
export * from './apitypes';
