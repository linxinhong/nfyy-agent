// Package os provides the AgentOS HTTP server and route handlers.
//
// # Security red lines
//
// Authentication/authorization configuration (auth mode, tenant isolation,
// scopes, security key — carried by AuthorizationConfig) is a
// process-startup-only concern. It must never move into a runtime-mutable
// configuration surface: a compromised admin session must not be able to
// change authentication itself. The full constraints live in
// docs/security-redlines.md (permissions are always computed fresh, never
// materialized; shareable credentials are stored hashed only; restricted
// views use default-deny wrappers).
package os
