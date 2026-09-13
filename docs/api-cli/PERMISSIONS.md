# Cortex permission requirements

Permissions are declared per operation in [`../generated/functional-coverage.md`](../generated/functional-coverage.md). A row records whether the boundary is public, session, capability, service or cluster-node, plus the exact capability and API-token scopes where applicable.

The CLI does not provide a privileged bypass: it calls the same HTTP/application authorization surface. Revoked/disabled sessions, tokens and node credentials remain subject to product authorization checks. Mutations/destructive operations declare audit events, and service/node actors remain distinct from human actors.
