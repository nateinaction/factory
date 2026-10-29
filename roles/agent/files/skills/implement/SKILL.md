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

Commit your work to the current branch. Do not push and do not open a pull request: foreman does both, from the run's output file. Write that file as the run's prompt describes, including the commit message and, on a first run, the pull request title and body. On a review round it also carries a reply per finding.
