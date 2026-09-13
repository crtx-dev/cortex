# Cortex functional API and CLI

Phase 5 certifies **31 declared operations**, with **29 website-used operations** and **29 observed CLI mappings**. The authoritative generated statement is [`../generated/functional-coverage.md`](../generated/functional-coverage.md); CI checks it byte-for-byte against the tested manifest.

This directory documents the stable automation surface: CLI grammar, HTTP API mapping, authentication, permissions, exit codes and schema/version policy. Browser-only, streaming and service-protocol exceptions are explicitly classified in the generated matrix rather than counted as missing CLI coverage.
