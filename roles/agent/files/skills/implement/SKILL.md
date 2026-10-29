---
name: implement
description: "Implement a piece of work based on a spec or set of tickets."
disable-model-invocation: true
---

<!-- Forked from mattpocock/skills skills/engineering/implement at 49dd158d1076134a641b33efb035946536778336. -->

Implement the work described by the writer in the issue given under "Issue material". The issue is the ticket: there is nobody to ask, so an ambiguity in it is a finding to work around, not a question to put back.

Call the Skill tool with "tdd" where possible, at the seams the spec implies.

Run typechecking regularly, single test files regularly, and the full test suite once at the end.

Once done, call the Skill tool with "code-review" to review the work.

Leave your changes uncommitted in the clone. Do not commit, push, comment or open a pull request: foreman does all of it, from the run's output file. Write that file as the run's prompt describes: `commit_message` and, on a first run, `pr_title` and `pr_body`. On a review round it also carries `replies`, one per finding, each with the finding's `finding` key, a `status` of `fixed` or `contested`, and a `reason`.
