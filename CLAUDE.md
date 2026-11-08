# CLAUDE.md

Instructions for AI agents working in this repo. See README.md for what the repo is and how to build and test it.

## Commits

- Don't add `Co-Authored-By:` trailers (or any other AI attribution) to commit messages. `.claude/settings.json` turns off Claude Code's default commit trailer; follow the same rule with any other tool.

## Public repo

This repo is public. Before pushing, check that nothing secret or private is staged: tokens (`smb_…`, `smc_…`), `.envrc`, private LAN addresses, node pubkeys of real hosts. `docs/live-testing.md` stays local (it's gitignored).

## Release signing

Releases are signed in CI (`cmd/signrelease`, secret `MESHBRIDGE_RELEASE_SIGNING_KEY`) and bridges auto-update only to releases that verify against the public key in `internal/update`. The private key's only local copy is `.secrets/` (gitignored): never commit, print or log it. Changing the key means shipping a release, signed with the old key, that trusts the new one.

## Changes that touch the relay

The relay library is deployed by `sierragridteam/sierra-data` (`cmd/mesh-relay`), which deploys from its `main`. Ship a relay change by tagging a release here, then bumping `github.com/sierragridteam/sierra-meshrelay` in sierra-data through a pull request. Never push to sierra-data's `main`.
