# Static analysis for Go correctness beyond golangci-lint

Date: 2026-10-30. Scope: `foreman/` (about 19.5k non-test Go lines, Go 1.27, Ginkgo/Gomega tests), where much of the code is written by coding agents.

Convention note: `docs/research/` did not exist; README.md says research findings are committed under `docs/research/`, so this file lives there.

Method: every claim below cites the tool's own docs, repo, pkg.go.dev page or API record. Where a primary page did not state something, the text says so rather than filling the gap. Repo facts (not cited by URL) come from reading `.pre-commit-config.yaml`, `flake.nix`, `foreman/.golangci.yml`, `foreman/go.mod`, `.github/workflows/check.yml` and `docs/adr/`.

## 1. What the repo already runs (do not duplicate)

| Area | In use |
|---|---|
| Hooks | go fix, go mod tidy, golangci-lint, `deadcode -test`, ginkgo with `--race --randomize-all`, ansible and yaml linters (`.pre-commit-config.yaml`) |
| CI | the same pre-commit hooks, plus `govulncheck ./...` (`.github/workflows/check.yml`) |
| golangci-lint | `default: standard` (errcheck, govet, ineffassign, staticcheck, unused) plus about 70 linters, including gosec, nilerr, nilnesserr, nilnil, errorlint, wrapcheck, exhaustive, bodyclose, noctx, contextcheck, spancheck, depguard (pure-core and CLI-edge rules), modernize, gocritic, revive, unparam, forcetypeassert (`foreman/.golangci.yml`) |
| Nix dev shell | go_1_27, golangci-lint, govulncheck, pre-commit (`flake.nix`); `deadcode` and `ginkgo` come in through the `tool` block of `foreman/go.mod` |
| Absent | fuzz tests, property tests, goroutine-leak checks, mutation testing, nilaway, semgrep/CodeQL, any custom analyzer (grep of `foreman/` for `func Fuzz`, `rapid.`, `goleak`: no hits) |

Goroutines and channels exist in non-test code in `internal/proxy/proxy.go` and `internal/egress/egress.go`, and sync or channel use in `internal/openrouter`, `internal/telemetry` and `internal/ghtest`. That is what makes the concurrency tools below relevant.

## 2. Gaps inside the tools already installed (cheapest wins)

### 2.1 go vet analyzers that golangci-lint leaves off by default

golangci-lint documents its govet default set as: appends, asmdecl, assign, atomic, bools, buildtag, cgocall, composites, copylocks, defers, directive, errorsas, framepointer, httpresponse, ifaceassert, inline, loopclosure, lostcancel, nilfunc, printf, shift, sigchanyzer, slog, stdmethods, stringintconv, structtag, testinggoroutine, tests, timeformat, unmarshal, unreachable, unsafeptr, unusedresult. The full list it can enable adds nilness, shadow, unusedwrite, waitgroup, hostport, stdversion, sortslice, reflectvaluecompare, deepequalerrors, atomicalign, fieldalignment, findcall and httpmux. Source: https://raw.githubusercontent.com/golangci/golangci-lint/main/.golangci.reference.yml (the `govet` block). The repo's `.golangci.yml` sets no govet options, so the non-default ones are off.

Analyzer descriptions are from the x/tools package index, https://pkg.go.dev/golang.org/x/tools/go/analysis/passes (v0.52.0), and each analyzer's own page.

| Analyzer | Bug class | Overlap with an enabled linter | False-positive cost | Verdict |
|---|---|---|---|---|
| `nilness` | "reports nil pointer dereferences, degenerate nil pointers, and panics with nil values", using SSA control flow; it notes degenerate comparisons "are often a mistake, especially in control flow related to errors" (https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/nilness) | Partial. golangci-lint describes `nilnesserr` as built on nilness and nilerr (https://golangci-lint.run/docs/linters/), so the error-return slice is covered, but the nil-deref and impossible-comparison parts are not enabled | Low. It reasons only about facts provable inside one function | Adopt |
| `unusedwrite` | "writes to struct fields and arrays that are never read": range-variable copies such as `for _, v := range xs { v.x = 1 }` and writes to a value receiver (https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/unusedwrite) | None found in the enabled list. `wastedassign` and `ineffassign` cover plain variables, not field writes on copies | Low | Adopt |
| `waitgroup` | `WaitGroup.Add` called inside the new goroutine, which races with `Wait` (https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/waitgroup) | None | Very low | Adopt (cheap; the repo spawns goroutines) |
| `stdversion` | standard library symbols newer than the file's Go version (package index above) | None | Very low | Adopt only if go.mod's `go` line could ever lag the toolchain; otherwise skip |
| `deepequalerrors`, `reflectvaluecompare`, `sortslice`, `hostport` | narrow misuse patterns (package index above) | `errorlint` covers error comparison but not `reflect.DeepEqual` on errors | Very low | Adopt as a batch; no cost |
| `shadow` | an inner-scope variable with the same name and type as an outer one, where the outer one is used afterwards. The doc says "the module generates too many false positives and is not yet enabled by default" (https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/shadow) | None. `predeclared` shadows only predeclared identifiers | Medium to high on `err` idioms | Try, in non-strict mode. It catches the classic `err :=` inside a loop returning a stale outer `err` |
| `fieldalignment` | structs that would use less memory with reordered fields (package index above) | None | High noise, and it is a memory optimization, not a correctness check | Skip |

How: add to `.golangci.yml`:

```yaml
linters:
  settings:
    govet:
      enable: [nilness, unusedwrite, waitgroup, deepequalerrors, reflectvaluecompare, sortslice, hostport, stdversion]
```

The `enable` key is "in addition to default", per the reference file above. No new package, hook or Nix change is needed.

### 2.2 gopls analyzers

gopls ships the x/tools analyzers plus its own, documented at https://raw.githubusercontent.com/golang/tools/master/gopls/doc/analyzers.md (entries include `nilness`, `shadow`, `unusedfunc`, `unusedparams`, `unusedvariable`, `unusedwrite`, `waitgroup`). It is an editor tool, not a CI gate. The only extra-to-lint ones worth noting are `unusedfunc` and `unusedparams`, which `deadcode` and `unparam` already cover. Skip as a CI tool; nothing to add.

### 2.3 staticcheck extras

golangci-lint's staticcheck default is `["all", "-ST1000", "-ST1003", "-ST1016", "-ST1020", "-ST1021", "-ST1022"]` (reference file above). Staticcheck's own non-default checks are SA9003, ST1000, ST1003, ST1016, ST1020, ST1021, ST1022 and ST1023 (https://staticcheck.dev/docs/checks/). So every correctness (SA) check except SA9003 (empty branch) is on already; the excluded ones are naming and doc style. Skip: there is nothing left to enable.

### 2.4 Linters inside golangci-lint that are not enabled

These are not external tools, but they are the nearest "free" correctness checks, and they are not in `.golangci.yml`. All names confirmed present in the reference file above.

- `exhaustruct_v5`: reports struct literals that omit fields. Implicit mode checks every literal, `-explicit` restricts it to marked types; v5.2.0 released 2026-09-04 (https://github.com/GaijinEntertainment/go-exhaustruct, release via https://api.github.com/repos/GaijinEntertainment/go-exhaustruct/releases/latest). A missing field is a typical agent-written bug (a config or options struct that silently takes a zero value). Try in explicit mode for config and request structs only; implicit mode would be noisy.
- `gochecksumtype`: switch exhaustiveness for sum types (https://golangci-lint.run/docs/linters/). It matters only if the code uses sealed-interface sum types, which `code-style` prefers; `exhaustive` already covers enums.
- `forbidigo`, `gomoddirectives`, `gomodguard_v2`: project-rule enforcement and `go.mod` hygiene (replace directives, banned modules). `gomodguard_v2` and `gomoddirectives` help against hallucinated or swapped dependencies (see section 6).
- `paralleltest`, `tparallel`, `thelper`: test-structure linters, mostly for `testing.T` tests; the suite is Ginkgo, so low value. Skip.

## 3. Whole-program and flow analysis

### nilaway (Uber)

- Catches: "potential nil panics" tracked across function and package boundaries, including receivers, interfaces, type assertions and type switches; the README says it catches "most", not all (https://github.com/uber-go/nilaway).
- Overlap: only partial with `nilerr`, `nilnil`, `nilnesserr`; those check specific return patterns, nilaway follows nil values through calls. It is not in golangci-lint's linter list (checked against the reference file), and the README says it is not built in because it "can report false positives"; the supported route is a custom golangci-lint binary via the module plugin system (`.custom-gcl.yml`, `golangci-lint custom`), or running the standalone checker (https://raw.githubusercontent.com/uber-go/nilaway/main/README.md, https://golangci-lint.run/docs/plugins/module-plugins/).
- Maturity: README says "under active development: false positives and breaking changes can happen". The repo has no GitHub releases or tags; last commit 2026-09-18; about 3.9k stars; go.mod requires Go 1.26; supports "the last two major versions of Go" (GitHub API https://api.github.com/repos/uber-go/nilaway, README above). nixpkgs packages a snapshot dated 2025-03-07 (`nix eval nixpkgs#nilaway.version` against this flake's lock gave `0-unstable-2025-03-07`), which is older than the repo's Go 1.27 toolchain and may not build against it; unverified.
- False-positive cost: medium; the README recommends `-include-pkgs=<module prefix>` to avoid noise from dependencies.
- Run: `go install go.uber.org/nilaway/cmd/nilaway@latest` then `nilaway -include-pkgs="github.com/nateinaction/factory/foreman" ./...`.
- Verdict: try, as an advisory, non-blocking CI job first. Skip as a pre-commit hook until it has a tagged release and a current nixpkgs package. Do 2.1 (`nilness`) first; it takes five minutes and nilaway's marginal value is measured against it.

### CodeQL (Go)

- Catches: dataflow and taint queries (command, SQL and log injection, disabled certificate checks, weak keys) plus quality queries such as `go/missing-error-check`, `go/useless-assignment-to-field`, `go/unexpected-nil-value`, `go/impossible-interface-nil-check`, `go/duplicate-condition`, `go/redundant-recover` (https://codeql.github.com/codeql-query-help/go/). The `security-and-quality` suite adds "extra maintainability and reliability queries" (same page).
- Overlap: security with `gosec` (enabled) but CodeQL does interprocedural taint, which gosec's pattern rules do not; quality queries overlap staticcheck and govet heavily.
- Maturity: github/codeql pushed 2026-10-30 (https://api.github.com/repos/github/codeql); nixpkgs has `codeql` 2.27.2. The `codeql` CLI's licence restricts use to open-source and research code on GitHub-hosted analysis; I did not verify the current terms, so check before using on a private repo.
- Run: GitHub's code-scanning workflow, not pre-commit (a database build is slow).
- Verdict: try as a scheduled or PR job only if the repo is public or has code scanning enabled; the main value over what exists is taint tracking through the HTTP proxy and egress code. Skip otherwise.

### Semgrep

- Catches: pattern and taint rules in YAML. Go is "Generally available" in Semgrep Code with cross-file dataflow and 80+ Pro rules (https://docs.semgrep.dev/supported-languages); that page does not say which of those need the paid tier, so assume cross-file needs it.
- The community `semgrep-rules` repo's `go/` directory holds framework rules only (aws-lambda, gorilla, gorm, grpc, jwt-go, lang, otto, template) (https://api.github.com/repos/semgrep/semgrep-rules/contents/go). This repo uses none of those frameworks, so the rule packs add little beyond gosec.
- Maturity: semgrep v1.183.0 released 2026-10-29 (https://api.github.com/repos/semgrep/semgrep/releases/latest); nixpkgs `semgrep` 1.180.0.
- Verdict: skip the rule packs. Use semgrep only as a vehicle for 2-3 repo-specific rules (see ast-grep below), and only if ast-grep is not chosen.

### ast-grep for project-specific rules

- A tree-sitter based "hybrid of grep, eslint and codemod" with YAML lint rules, Go among the supported languages (https://ast-grep.github.io/guide/introduction.html). That page does not discuss type information; treat rules as syntactic. I did not verify its limits further.
- Release 0.51.0 on 2026-10-15 (https://api.github.com/repos/ast-grep/ast-grep/releases/latest); nixpkgs 0.51.0.
- Overlap: `forbidigo`/`depguard` cover identifier and import bans already. ast-grep adds structural rules ("every `exec.Command` in package X must be preceded by...").
- Verdict: skip for now. Prefer `forbidigo` and `depguard` (already installed) until a rule needs structure.

### Custom go/analysis analyzers

- The `analysis` package defines `Analyzer`, `Pass` (syntax, types, positions), `Diagnostic`, cross-package facts, an `analysistest` subpackage using `// want` comments, and `singlechecker`/`multichecker` for standalone binaries (https://pkg.go.dev/golang.org/x/tools/go/analysis). golangci-lint v2 loads such analyzers as module plugins (https://golangci-lint.run/docs/plugins/module-plugins/).
- Verdict: adopt only when a repeated agent mistake cannot be expressed by `forbidigo`/`depguard`/`gocritic` rules. The stable, typed approach beats prompts for rules that "can't have exceptions", but it costs a Go module to maintain. Not first-wave.

## 4. Dependencies and supply chain

- `govulncheck`: already in CI and the dev shell. Uses static analysis of source or a binary's symbol table to report only vulnerabilities that could affect the code; source mode accepts `-test`; JSON/SARIF/OpenVEX output exit 0 regardless of findings; function-pointer and interface calls are handled conservatively, so false positives are possible (https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck). The repo runs `govulncheck ./...` without `-test`, so vulnerabilities reachable only from test code are not reported; adding `-test` is optional. Last release v1.1.4 (2025-01-13), main branch updated 2026-10-11 (https://api.github.com/repos/golang/vuln). The CI job runs only on push and PR; the same pkg.go.dev page and the database change independently of the code, so a weekly scheduled run would catch new advisories on an idle repo (my inference from how the database works, not a quoted recommendation).
- `osv-scanner` (nixpkgs 2.7.0) and `gitleaks` (nixpkgs 8.30.1): a second vulnerability source and a secret scanner. gitleaks is relevant because the house rules treat a committed secret as a bug and a `secrets/` directory exists in the tree. I did not read either tool's docs in this pass, so I make no claim beyond availability. Try gitleaks as a pre-commit hook; skip osv-scanner (govulncheck's reachability is more precise for Go).
- `apidiff` / `gorelease`: gorelease is "an experimental tool that helps module authors avoid common problems before releasing a new version of a module" and is "eventually intended to be merged into the go command" (https://pkg.go.dev/golang.org/x/exp/cmd/gorelease). apidiff "determines whether two versions of a package are compatible" (https://pkg.go.dev/golang.org/x/exp/cmd/apidiff). Both guard importable library APIs. `foreman` is a binary deployed by Ansible and everything is under `internal/`, so there are no external importers. Skip.

## 5. Dynamic complements

| Tool | Class of bug | Status in repo | Verdict |
|---|---|---|---|
| Race detector (`-race`) | Concurrent unsynchronized access, loop-variable capture, `err` accidentally shared, concurrent map writes, send/close races. Only races that execute during the run; memory 5-10x and time 2-20x (https://go.dev/doc/articles/race_detector) | Already on in the ginkgo hook | Keep. Existing, so no change |
| Native fuzzing | Panics, test failures, `os.Exit`, stack overflow, and executions over 1 second (hangs). Seed corpus in `testdata/fuzz/` runs under plain `go test`; failures are minimized and kept as regression files; `-fuzztime` bounds a run (https://go.dev/doc/security/fuzz/) | None | Adopt for parsers and decoders of untrusted input: the egress proxy's request and host parsing, `internal/prompt`, `internal/config`. Seed-corpus runs ride the existing test hook for free; run `-fuzz -fuzztime=60s` on a schedule. Fuzz targets must be fast and deterministic, with no global state (same page) |
| Property-based testing, `pgregory.net/rapid` | Invariants over structured inputs, with automatic shrinking; state-machine tests for stateful code; no coverage guidance; `MakeFuzz` converts to a fuzz target (https://raw.githubusercontent.com/flyingmutant/rapid/master/README.md). v1.3.0 2026-04-30, active (https://api.github.com/repos/flyingmutant/rapid) | None | Try for pure-core packages (`internal/workflow`, `internal/opencode`, `internal/sum`) where round-trip and idempotence properties exist. This fits the "every effect safe to apply twice" rule |
| Goroutine-leak detection | Goroutines left running after a test. `goleak` offers `VerifyNone`/`VerifyTestMain`, but cannot reliably tell leaks from unfinished tests under `t.Parallel` (https://raw.githubusercontent.com/uber-go/goleak/master/README.md); last release v1.3.0 (2023-10), repo pushed 2026-09-15 (https://api.github.com/repos/uber-go/goleak). Since the suite is Ginkgo, Gomega ships `gleak` (inspired by goleak, `Eventually(Goroutines).ShouldNot(HaveLeaked(...))`) (https://pkg.go.dev/github.com/onsi/gomega/gleak); gomega is already a dependency (v1.44.0) | None | Adopt `gleak` in the proxy and egress specs, which start goroutines. No new module. Use `IgnoreGinkgoParallelClient()` if specs run with `ginkgo -p` (same page) |
| Mutation testing | Tests that execute code but never assert on it, which is the characteristic weakness of agent-written tests | None | Try, once, as an audit, not a gate. See below |

Mutation tools, with their status:

- `avito-tech/go-mutesting`: arithmetic, loop, conditional, branch, expression and statement mutators; mutation score = killed/total; config via YAML; suppression comments; the README itself notes false positives (an equivalent mutant such as an early exit) (https://raw.githubusercontent.com/avito-tech/go-mutesting/master/README.md). v2.3.1 on 2025-12-26, commit 2026-10-02 (https://api.github.com/repos/avito-tech/go-mutesting). The original `zimmski/go-mutesting` was last pushed 2024-07-04 (v1.2, 2021) (https://api.github.com/repos/zimmski/go-mutesting), so use the avito fork. Not in nixpkgs (`nix eval` failed for `go-mutesting`); install with `go install`.
- `go-gremlins/gremlins`: PITest-inspired; statuses KILLED, LIVED, NOT COVERED, TIMED OUT; "designed for smallish Go modules" because runs "can take hours"; 0.x with config flags allowed to change between minors (https://raw.githubusercontent.com/go-gremlins/gremlins/main/README.md). v0.6.0 on 2025-12-06, pushed 2026-06-26 (https://api.github.com/repos/go-gremlins/gremlins). Not in nixpkgs.
- Verdict: try go-mutesting on the pure packages only, as a one-off report, and read the surviving mutants. Skip as a hook or CI gate: the cost is hours and the survivors need human judgement.

## 6. Architecture and dependency rules

- `depguard`: already enabled with a pure-core rule (no `os`, `os/exec`, `net`, `net/http` in `internal/workflow` and `internal/opencode`) and a cobra/viper edge rule.
- `go-arch-lint` (fe3dback): maps packages to components and compares the actual import graph with a declared `mayDependOn` graph in `.go-arch-lint.yml`; `check` exits 1 on violations; install via Docker, `go install` (Go 1.25+) or release binaries (https://raw.githubusercontent.com/fe3dback/go-arch-lint/master/README.md). v1.19.1 released 2026-10-29 (https://api.github.com/repos/fe3dback/go-arch-lint); nixpkgs 1.19.0. It adds what depguard cannot: whole-graph rules ("`internal/github` may not import `internal/egress`") instead of per-package deny lists. The README does not compare itself to depguard, so that difference is my reading of the two config models.
- Verdict: try, once the package graph stabilises. Today depguard already encodes the two rules ADR 0001 and 0002 imply; the pure/edge split is the only boundary the repo enforces. Until more boundaries are written down, extending the depguard rules is cheaper.
- `arch-go` is an alternative (v2.1.2, 2026-02-03; https://api.github.com/repos/arch-go/arch-go); no reason to prefer it.

## 7. Guardrails specific to agent-written code

Primary evidence on the failure modes:

- Generated code is vulnerable often enough to gate on: in a controlled study of Copilot, about 40% of 1,689 generated programs across 89 scenarios were vulnerable (https://arxiv.org/abs/2108.09293).
- Models invent dependencies: across 16 code models and 576,000 samples, at least 5.2% of recommended packages were hallucinated for commercial models and 21.7% for open-source models, with 205,474 unique invented names (https://arxiv.org/abs/2406.10279). Go resolves imports by module path, so a hallucinated path is a fetch of whatever a stranger registered. Mitigation within the tools above: `go mod tidy` and `go mod verify` in the hook (tidy is already there), `gomodguard_v2`/`gomoddirectives` in golangci-lint, and review of every `go.mod`/`go.sum` diff by a human.

Design implications (reasoning from the above, not quoted from a source):

1. Put every check in the deterministic gate (pre-commit plus CI), which the repo already does, rather than in prompt instructions. Adding analyzers to the existing hook set costs nothing in agent instructions.
2. Verify the tests as well as the code: the agent writes both, so a green suite proves little. Mutation testing, fuzzing and property tests are the checks that do not trust the test author. The mutation audit in section 5 is the most direct check on this.
3. Treat `//nolint` as a review item; `nolintlint` is already enabled.

## 8. Shortlist, ranked

1. Enable the extra govet analyzers (`nilness`, `unusedwrite`, `waitgroup`, `deepequalerrors`, `reflectvaluecompare`, `sortslice`, `hostport`, `stdversion`) in `.golangci.yml`. One config edit, no new tool, no new dependency.
2. Add Go fuzz targets for the untrusted-input parsers (egress and proxy request handling, `prompt`, `config`). Seed corpora run under the existing ginkgo/go test hook; time-boxed `-fuzz` on a schedule.
3. Add Gomega `gleak` leak checks to the proxy and egress specs. No new module.
4. Run `exhaustruct_v5` in explicit mode on config and option structs, and `gomodguard_v2`/`gomoddirectives` for dependency hygiene (all inside golangci-lint).
5. Try, as advisory only: `nilaway` (standalone, `-include-pkgs`), a one-off `go-mutesting` audit of the pure packages, `gitleaks` as a hook, `rapid` properties in the pure core. Defer CodeQL, semgrep packs, ast-grep, go-arch-lint, custom analyzers and apidiff/gorelease until a concrete need appears.

## 9. Not verified

- CodeQL licensing terms for private repositories.
- Whether the nixpkgs `nilaway` snapshot builds with Go 1.27.
- gitleaks and osv-scanner behaviour beyond their nixpkgs presence; I did not read their docs.
- Semgrep Community Edition's exact Go feature set.
- Whether any enabled analyzer in section 2.1 produces findings in this codebase; I did not run them, since the task was research only.
