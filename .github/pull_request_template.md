<!--
Conventional commit titles, please — they drive the generated release notes.
e.g. fix(unifi): hold state when the UPS drops off the controller
-->

## What and why

<!-- What changes, and what problem it solves. Link the issue: Closes #NN -->

## How it was verified

<!--
What you actually ran. `make dev-mock` counts; so does "envtest only".
If you could not verify something — no hardware, no failover to observe,
behaviour that depends on another in-flight change — say so explicitly below
rather than leaving it implied.
-->

- [ ] `make test`
- [ ] `make lint`
- [ ] `make manifests generate` output committed (CI fails on drift)

## Not verified

<!--
Anything documented or changed that you could not confirm first-hand, each with
what to do and what to look for. Delete this section if there is nothing.
-->

## Checklist

- [ ] Conventional commit messages; one commit per issue if this closes more than one
- [ ] Behaviour changes have a test
- [ ] No raw API response committed — fixtures come only from `hack/capture-*.sh` with its field allowlist
- [ ] No provider-specific logic added to `internal/engine/`
- [ ] Docs updated if behaviour, state keys, or chart values changed
- [ ] Breaking change marked with `!` and the migration explained above
