# The GitHub emulator in foreman's tests

Foreman's tests run against [vercel-labs/emulate](https://github.com/vercel-labs/emulate), a
stateful GitHub REST API emulator, instead of hand-stubbed endpoints. The dev shell pins
`emulate` 0.12.3 from `nix/emulate/` (package.json plus lock file), so tests need no global
install and no network:

```sh
nix develop --command pre-commit run --all-files
```

Run `go test` from the same dev shell — the suite starts `emulate` from `PATH`.

## How a spec runs

- The package's `BeforeSuite` starts one emulator for the whole test binary
  (`ghtest.StartEmulate`) with `foreman/emulate.config.yaml`. Each package gets its own process;
  starting one per spec would cost a Node startup each time.
- Each spec gets a fresh repository, `o/r`, created and deleted through the emulator's API by
  `newHarness`. Built-in services have no reset over the CLI, so a fresh repository is the
  isolation boundary.
- Setup calls (create repository, issue, pull request, review) go directly to the emulator with
  the seeded `admin-token`. They bypass the proxy, so they do not appear in request assertions.
- Foreman's client points at `internal/ghtest`: a recording proxy in front of the emulator. It
  records every request (`Calls`, `Summary`, `Bodies`, `Unexpected`, including the
  `Authorization` header) and forwards unmatched requests to the emulator.
- Tests read outcome state back from the emulator where that is clearer than reading a request
  body. Issue #5 is fixed because many flow specs name it, so the harness creates filler issues
  below it; the pull request number follows the repository's sequence and is read back with
  `h.pullRequest` instead of assuming `#8`. A retro is a marker comment on the issue it worked, so
  its number is the issue's: reads it back with `h.retroComments` and `emulator.Comments`.

## What emulate cannot do, and how the tests handle it

emulate deliberately answers from state: it has no fault injection and does not record requests
for built-in services. `internal/ghtest` fills both gaps, and the tests keep their old coverage.

| Gap | Handling |
| --- | --- |
| No way to force a 500/502/422 | The proxy registers a route for the call and answers with the status (`Proxy.Status`), or forwards after the first refusal (`Proxy.Handle` + `Proxy.Forward`). This is how the poll failure, label-delete failure, approval fallback and review failure specs still force GitHub errors. |
| No request log for built-in services | The proxy records method, path, query, headers and body. `fakegh`'s assertion helpers moved to the proxy with no loss. |
| No seed for issues, labels, comments, reviews or pull requests | Specs create them through the API. Labels are created implicitly when an issue is created with them. |
| No reset for built-in services | One process per package; `newHarness` deletes and recreates `o/r` for every spec. |
| The issues list omits pull requests | GitHub includes pull requests in `GET /issues`; emulate filters them out. `PickIssue`'s "skip a pull request" branch is therefore not exercised against emulate. The old spec that faked a pull request in the issue list was removed; the behaviour remains covered by `workflow`'s unit tests. |
| No "an App cannot approve its own pull request" rule | The approval → comment-review fallback is still covered by forcing the 422 in the proxy, not by emulator state. |
| No check runs, commit statuses or Actions job logs | The CI gate's reads have no emulator state to answer from, so the specs register the `GET /repos/o/r/commits/…` subtree and the job-log path on the proxy and answer with the check runs, statuses or log text the case needs. `Proxy.Handle` on a trailing-slash pattern covers every commit SHA. |
| Seed numbering starts at 1 per repository | Where a fixed number matters (issue #5), the harness creates filler issues below it. Pull request and retro numbers are read back rather than hard-coded. |

## Authentication

`emulate.config.yaml` seeds the users and tokens the tests use:

- `admin-token` (login `o`): repository and state setup, never seen by the client under test.
- `secret-token` (login `factory`): the GitHub client specs.
- `repo-token` (login `factory`): the Reviewer's token file in flow specs.
- `minted-token-implementer` and `minted-token-reviewer`: what the faked `github-app-token`
  helper returns, one login per agent role. `expectAuthorizedAs` checks these on recorded calls.

The emulator allows any authenticated user to write a public repository, so the role tokens need
no collaborator setup. Foreman's client never calls the App installation endpoints; the helper
process is still faked, so emulate's App/JWT model is not on this path.
