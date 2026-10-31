# Code Standards

What language to write code in when this repo needs code.

## Default to Go

If a task needs code, write it in Go. Put it where the rest of the Go lives (`foreman/`) and test it with the Ginkgo/Gomega specs beside it.

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
