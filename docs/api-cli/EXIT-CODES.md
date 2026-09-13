# CLI exit codes

| Code | Meaning |
| ---: | --- |
| 0 | success |
| 1 | operation/response failure |
| 2 | usage, invalid input or missing destructive confirmation |
| 3 | authentication or authorization failure |
| 4 | not found |
| 5 | conflict |
| 6 | network timeout/unavailable dependency |
| 7 | partial success for distributed/fan-out commands |

Scripts should branch on the exit code and parse `--json`; do not scrape human-formatted output.
