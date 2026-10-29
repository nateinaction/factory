# Is [#56](https://github.com/nateinaction/factory/issues/56) possible? Replacing `fakegh` with vercel-labs/emulate

**Answer:** mostly. Porting foreman's GitHub tests onto
[emulate](https://github.com/vercel-labs/emulate) removes the per-endpoint hand-stubbing that
motivates the issue, but two of its acceptance criteria cannot be met as written:

- emulate cannot inject faults, so the tests that force 500/502/422 need a stub;
- emulate has no request log for built-in services, so `fakegh`'s recording assertions
  (`Summary`, `Bodies`, `Unexpected`, `Calls`) have nothing to read.

Everything else — the stateful behaviour foreman actually leans on — is there, and in one place
more faithful than today's fake.

Checked against emulate `main` at `8cf6bfc` (npm `emulate` 0.12.3) and foreman's
`internal/github`, `internal/foreman`.

## What foreman calls today

Every call in `foreman/internal/github/github.go` against what emulate implements:

| foreman call | emulate | Notes |
| --- | --- | --- |
| `GET /repos/:o/:r/issues?state=open&labels=X&sort=created&direction=asc&per_page=20` | yes | label filter is AND, `sort`/`direction` honoured, `Link` headers, `per_page` capped at 100 (= `github.PageSize`) |
| `GET /repos/:o/:r/issues/:n` | yes | |
| `GET /repos/:o/:r/issues/:n/comments` (list and create) | yes | |
| `PATCH /repos/:o/:r/issues/comments/:id` | yes | |
| `POST /repos/:o/:r/issues/:n/labels` (`{"labels":[…]}`) | yes | |
| `DELETE /repos/:o/:r/issues/:n/labels/:name` | yes | answers **404 when the label is not on the issue**, which is exactly the claim race `Claim`/`Finish` are written for. Today `fakegh` has to fake that with `Status(…, 404)` |
| `POST /repos/:o/:r/issues` (with labels) | yes | the labels themselves must exist on the repo first |
| `GET /repos/:o/:r/pulls?state=open&head=o:branch` | yes | `owner:ref` matching is implemented |
| `POST /repos/:o/:r/pulls` | yes | creates the head branch from the default branch tip; the repo must not be empty (`auto_init`, on by default) |
| `GET /repos/:o/:r` → `default_branch` | yes | |
| `GET /repos/:o/:r/pulls/:n/reviews` (paged) | yes | ordered by id ascending, `page`/`per_page`, `Link` header |
| `POST /repos/:o/:r/pulls/:n/reviews` (`event` APPROVE / COMMENT) | partial | the review is created, but emulate does **not** model "a GitHub App cannot approve the pull request it authored", so `PostReview`'s 422 → comment-review fallback never fires against it |

## Gaps

### No fault injection

`fakegh.Status` is used to force failures:

- `internal/foreman/app_test.go` — 500 on the issues list (poll error path);
- `internal/github/github_test.go` — 500/502 on label delete, 422 on review approve;
- `internal/foreman/run_test.go` — the approve → comment-review fallback.

emulate answers from state only; nothing makes an endpoint return 500. These specs need either a
small hand-written `httptest` stub in `internal/github`, or a call-error seam in `Client`.

### No request recording

The only inspection route for a built-in service is `GET /_emulate/installation-tokens`; the
Inspector's request view is for custom emulators. So these lose their source (about 38 call sites
across six test files): `Summary()`, `Bodies()`, `Calls()`, `Unexpected()`, and the harness helpers
`expectAuthorizedAs`, `expectOnlyReads`, `expectNoWork`.

State assertions replace most of them:

- labels added, comments posted, reviews posted → read them back off the issue or pull request;
- "authorized as the role's own App" → seed one login per role token and assert the comment or
  review author;
- "did not write" → state unchanged.

"Made no call at all" is not observable.

### Thin seed config

`emulate.config.yaml` seeds `users`, `orgs` (with `members`), `repos`, `tokens` (an arbitrary token
string mapped to a login), `apps` and `oauth_apps`. It seeds **no** issues, labels, comments,
reviews or pull requests, so per-spec state is built over the API.

Issue and pull request numbers come from one sequence per repo (`getNextIssueNumber`), while
foreman's tests hardcode #5, #8 and #41. Tests either capture the returned numbers or start from a
fresh repo.

### No reset for built-in services

`reset()` exists on the programmatic emulator instance only; over the CLI, only `--watch` reloads
reset state (to the seed). Two workable isolations:

- one emulate process per `go test` package, and a **fresh repo per spec** (`spec-N/r`) — cheap,
  isolated, and sidesteps the numbering above;
- a fresh process per spec — simple but too slow to be worth it.

### Auth

Unknown tokens carry no user, and writes need one, so the harness seeds the exact token strings its
faked `github-app-token` helper already returns
(`{"minted-token-implementer": {login: implementer}, …}`) and leaves `fakeRunner` untouched. Public
repos are readable by any authenticated user and writable by any seeded user, so no collaborator
setup is needed.

### Fidelity divergences that remove tests

- emulate's issues list filters pull requests out (`!is_pull_request`), where GitHub returns them.
  `PickIssue`'s "skip pull requests" branch (`internal/github/github_test.go`) becomes untestable
  against it.
- No self-approval rule, as above.

### Provisioning

emulate is a Node package (Node 22/24+), so `flake.nix` needs `nodejs_22` plus a pinned emulate
(npm version with a hash, e.g. `buildNpmPackage`), and the seed belongs in the repo
(`foreman/emulate.config.yaml`). CI already runs the hooks under `nix develop`, so this keeps the
"no global installs" rule. One process per package costs about a second of startup.

## The issue's open questions

**Does emulate support the GitHub App auth flow (JWT → installation token)?** Yes, and foreman needs
none of it for this port: foreman's client never calls GitHub's App endpoints, `github-app-token`
does, and the tests fake that helper. emulate models seeded `apps` with `permissions` and
`installations` (including selected repositories), verifies RS256 JWTs, and serves
`GET /app/installations` and `POST /app/installations/:id/access_tokens`, enforcing the App's
permissions on writes.

**Process lifecycle:** one process per `go test` package, fresh repo per spec (see above).

**Mature enough to depend on?** It is a Vercel Labs experiment (Apache-2.0, ~2k stars, frequent
releases, 0.12.x). Pinning an npm version with a hash in the flake gives reproducibility; the cost
is a new Node toolchain in the test path.

## Recommended shape

Keep emulate for **state** and a ~50-line recording, fault-injecting `httptest` handler in front of
it — today's `fakegh.Server` minus routing. That removes per-endpoint stubbing (the stated
motivation), keeps `Summary`/`Bodies`/`Unexpected`/forced 500 and 422, and turns the "package
removed" criterion into "no route registration".

The alternative — pure emulate, assertions rewritten as state reads, a stub kept only for the error
paths — reads cleaner but loses "no unexpected call" coverage and weakens a few flow specs.

## Rough port

1. `flake.nix`: `nodejs_22` + pinned emulate; a `foreman/emulate.config.yaml` seed (two role tokens,
   public repos, one App per role if wanted).
2. Test harness: start emulate once per package; per spec create a repo, its labels and issues
   through the API, capturing numbers.
3. Rewrite assertions to state reads; keep the recorder if it is kept.
4. Move the forced-failure specs to the stub.
5. Delete `foreman/internal/fakegh`.

## How this was checked

By reading foreman's client and tests against emulate's source (`packages/@emulators/github`,
`packages/@emulators/core`) and its docs. Nothing was run: no Node toolchain is present in the dev
shell today, so the flows above are unverified against a live emulator. The first step of any port
should be a spike that starts emulate and replays one foreman spec end to end.
