# IAM authorized context, version 1

This is the additive authorization contract used by the Helix app_core module candidate. The canonical wire definition is `contracts/modulerpc/v1/modulerpc.proto`; Go and Rust consume that definition. Existing role-only modules have not been automatically migrated by this addition.

## Authority and enforcement

IAM alone evaluates policy. Modules declare operation names and resource bindings, then match exact IAM-issued grants. app_core verifies the signed credential and conveys `User.Access` over the authenticated Unix socket. Constructing an SDK value, decoding protobuf or supplying an HTTP header does not verify authority.

`AuthorizedContext` contains `version`, `issuer`, stable `subject`, `account`, `application`, `audience`, `policy_version`, `not_before`, `expires_at` and exact `permission`/`resource` grant pairs. Version 1 has no wildcard expansion. The module's expected issuer/account/application/audience and resource come from trusted configuration. Matching only the permission name is insufficient.

The Go `User.ValidateAccess`/`User.Authorize` and Rust `authorized` helpers enforce those bindings and the exclusive expiry boundary. Missing, unknown-version, wrongly bound or expired context grants nothing. Email and legacy roles are identity/display metadata, not a substitute grant set. Zero permissions can still describe an authenticated person; protected operations remain denied.

IAM binds context to the signed token and session validity. app_core rejects an embedded decision that disagrees with the signed subject, issuer, audience or tenant, or outlives the credential. Modules cannot extend its lifetime. On `/oauth/check`, `access_changed` forces a cookie to obtain a freshly signed token; an old bearer is refused and must obtain new IAM authority. Existing bounded outage behavior remains at the host: no new grant is minted and an already-issued context cannot outlive its expiry.

## IAM policy inputs

The first implementation reads IAM-owned JSON from the declared `MYCEL_IAM_PERMISSION_POLICY_FILE` at boot. Unknown fields, duplicate client bindings, incomplete bindings and wildcard grants fail startup. The example below is configuration, not an automatic grant or migration:

```json
{
  "version": "policy-7",
  "clients": [{
    "client_id": "helix-host",
    "audience": "helix-host",
    "application": "helix",
    "account": "account-1",
    "roles": {
      "knowledge-reader": [{"permission": "helix:read", "resource": "kb:one"}]
    },
    "service_grants": []
  }]
}
```

The directory or extension supplies person eligibility and role assignments. IAM applies its policy after an allow and never converts a source denial or unavailability into an allow. A configured client requires a nonempty canonical principal subject and matching account; no email fallback creates a new subject at this boundary. Unconfigured identity-only clients remain on their existing contract and cannot satisfy a module requiring `Access`.

Machines use `client_credentials` at IAM's token endpoint with their own registered client assertion, never app_core's reserved host key. The worker keeps its own registration/client identity; IAM service policy selects the target host audience. Unique relying-party registration audiences remain unchanged. Only an explicit IAM `service_grants` list can authorize that client. IAM issues stable `service:<client_id>` identity, marks the signed principal as a service, and re-evaluates its policy on check. Service tokens require fresh client proof to renew. Form scopes, person roles and old Helix API keys cannot enlarge a machine's grants.

Policy administration, policy persistence adapters and the complete cross-module rollout are separate delivery gates. This versioned configuration is not a claim that those interfaces or an AWS cutover already exist.

## Runtime

The Rust crate under `rust/` compiles the canonical protobuf at build time. It serves only `MYCEL_LISTEN=unix://...`, requires a private same-UID socket directory, checks peer UID, protects active/non-socket paths and recovers owned stale sockets. It applies the same 32 MiB message, 30 MiB body and 64 KiB header budgets. Shutdown drains within 15 seconds. Discovery, health and operation authorization remain separate concerns; licensing belongs to app_core.

For the Helix candidate, run `mycel_helix/scripts/app-core/verify.sh` in the Rust workspace. Native success is not Linux-image, Kubernetes or AWS deployment certification.
