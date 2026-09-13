# Cortex schema and version policy

Every automatable operation has named input/output schema identities in the generated Phase 5 matrix. The current operation-contract schema version is **1**. Schema names are compatibility identifiers for automation and generated coverage; changes that break an existing input/output contract require an explicit versioned migration rather than silently reusing the old name.

Protocol/browser/service operations may use protocol-specific payloads and are classified separately. The committed generated JSON matrix (`../generated/functional-coverage.json`) is the machine-readable coverage/version statement and is checked by CI.
