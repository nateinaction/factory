---
name: research
description: Investigate a question against high-trust primary sources and capture the findings as a Markdown file in the repo. Use when a research ticket needs its question answered from sources outside the working directory.
---

<!-- Forked from mattpocock/skills skills/engineering/research at 49dd158d1076134a641b33efb035946536778336. -->

Do the research yourself: the `task` tool is denied, so there is no background agent to hand it to.

Its job:

1. Investigate the question against **primary sources** (official docs, source code, specs, first-party APIs), not a secondary write-up of them. Follow every claim back to the source that owns it.
2. Write the findings to a single Markdown file under `docs/research/` in the repository, citing each claim's source. That directory is the only part of the clone you may change.
3. Write the run's output file as its prompt describes: the branch name, the resolution comment (a link to the findings and a short answer) and the map's one-line decision line. Never post or push anything yourself; foreman does both.
