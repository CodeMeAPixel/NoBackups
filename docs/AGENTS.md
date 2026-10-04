# Documentation project instructions

## About this project

- Mintlify docs for NoBackups, a Go backup agent for Linux servers (code lives in the parent directory)
- Pages are MDX files with YAML frontmatter; configuration lives in `docs.json`
- Preview with `make docs` from the repo root, validate with `make docs-check`

## Terminology

- "job": one backup definition (sources, destinations, schedule)
- "destination": a named place backups are stored (`type: s3` or `type: local`)
- "snapshot": one stored archive of a job, identified by its UTC timestamp, e.g. `20261004T031500Z`

## Style preferences

- Active voice, second person, sentence case for headings
- Light puns in intros and headings are on-brand; keep instructions themselves plain
- Don't assume a specific chat tool (Slack, Discord…) except where documenting that integration
- Keep config examples valid: they should pass `nobackups validate`

## Content boundaries

- Document behaviour as implemented in the Go code; check `internal/` when unsure
