# Cortex CLI reference

Functional commands use:

```text
cortex <resource> <verb> [path arguments] [options]
```

Common options are `--input file.json`, `--input -`, `--json`, `--output table|json|plain`, `--quiet`, `--timeout`, `--request-id`, repeatable `--query key=value`, `--url`, `--session-file`, `--token-file` where supported, and `--yes` for destructive operations.

The default origin is `http://127.0.0.1:7331`. The complete resource/verb list is generated in [`../generated/functional-coverage.md`](../generated/functional-coverage.md). Existing service/cluster/product-local command families remain available where documented by the product.
