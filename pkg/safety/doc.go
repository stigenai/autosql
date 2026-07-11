// Package safety provides deterministic, reusable migration risk analysis.
//
// Use Runner with Builtins to inspect a validated schema.ChangeSet. Add
// Statement values when rendered SQL is available; SQL itself is never emitted
// by reporters. Target statistics improve confidence but are optional. The
// PostgreSQL analyzer conservatively reports risks when statistics are absent.
//
// Suppressions match one stable rule ID and one resource ID. They always require
// an audit reason and can expire. Consumers should retain suppressed diagnostics
// for audit rather than deleting them. Thresholds turn operational estimates
// into errors, allowing a later policy gate to reject the plan.
package safety
