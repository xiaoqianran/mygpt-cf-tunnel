# OpenAPI contract policy

The deployed Custom GPT action contract is intentionally versioned and frozen. The current contract version is **0.6.0**.

`internal/agent/openapi.json` is a compatibility boundary, not a release-version file. Normal implementation, reliability, deployment, logging, timeout, performance, and documentation changes should **not** modify it.

Version 0.6.0 is an intentional Action-contract migration from 0.5.3. The existing four callable operations are unchanged. The result schemas add GPT Actions' official `openaiFileResponse` field plus metadata that distinguishes an inline preview from genuinely lost output. This migration is required because a normal Action JSON response cannot carry arbitrarily large stdout/stderr, while GPT Actions can ingest returned files.

The contract SHA-256 is locked by `TestOpenAPIContractIsFrozen`. After an intentional schema migration, review the complete diff, then update both the hash below and the test lock to the exact SHA-256 of `internal/agent/openapi.json`.

Current frozen SHA-256:

```text
66a83f4cff2ec59b2b49768235f47daa458e693e54b66e5d28a754b6bf718b42
```

The service implementation version may advance independently of the OpenAPI contract version. Existing MyGPT installations must refresh/re-import their Action schema after a contract migration so the Builder recognizes new response fields such as `openaiFileResponse`.

Before changing the contract, verify that the requirement cannot be implemented behind the existing four operations:

```text
POST /v1/command/run
POST /v1/command/start
GET  /v1/command/jobs/{id}
POST /v1/command/jobs/{id}/cancel
```

Prefer backward-compatible internal behavior, repository configuration, GPT instructions, and server-side implementation changes over adding or changing Action fields or operations.
