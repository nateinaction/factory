# Code Standards

What language to write code in when this repo needs code.

## Default to Go

If a task needs code, write it in Go and test it with the Ginkgo/Gomega.

## Write Go tests with Ginkgo

Write every Go test as a Ginkgo spec with Gomega matchers, not as a plain `func TestX(t *testing.T)`.

- Each package has one `_suite_test.go` that calls `RunSpecs`; the specs live in the package's other `_test.go` files.
- Name each `Describe`, `Context` and `It` for the behaviour it checks, so a failure reads as a sentence.
- Fuzz targets are the exception: `go test -fuzz` needs a plain `func FuzzX(f *testing.F)`.

## Put tests beside what they test

A test lives next to the thing it tests, never in a top-level `tests/` directory. A test that checks a contract between two components belongs to the side whose output it checks.

## Avoid shell scripts

Don't write `.sh` files, and don't grow inline shell in workflows, DAGs, Ansible tasks or hooks past a single command.

- Shell has no types, swallows errors by default and quotes badly. Every one of those is a bug waiting in a place nobody tests.
- If a step needs a loop, a conditional, parsing or a second pipe, it's code. Write it in Go.

## Avoid Python scripts

Don't write `.py` files, and don't reach for `python -c` or a heredoc to parse JSON, YAML or text.

- It adds a runtime and dependencies the Nix dev shell and the worker hosts would have to carry.
- Go is already built, linted and tested here. Python would be the only thing that isn't.

## When a script already exists

Leave existing shell and Python alone unless the task touches it. If it does, and the change is more than a line or two, port it to Go as part of that change rather than extending it.

## Exceptions

A one-line command in a workflow step, a Nix `shellHook`, or a pre-commit entry is fine. Say why in a comment if it's anything more.
