---
name: grilling
description: Grill the writer relentlessly about a plan, decision, or idea. Use when an issue needs stress-testing, or when a planning round must surface the decisions it depends on.
---

<!-- Forked from mattpocock/skills skills/productivity/grilling at 49dd158d1076134a641b33efb035946536778336. -->

Interview the writer relentlessly until you reach a shared understanding. Map this as a **design tree**: every decision branches into the decisions that hang off it.

Work the tree in **rounds**. The **frontier** is every decision whose prerequisites are already settled: the questions you can ask _now_ without guessing at answers you haven't heard yet. Ask the whole frontier in one round: number each question and give your recommended answer.

## In the factory

You cannot ask the writer interactively: the `question` tool is denied. A round **ends your run**. Write the round to the run's output file exactly as the run's prompt describes (the Planner's round goes in `questions`), then stop. The next run reads the writer's answers from the issue thread; they reshape the tree, and you recompute the frontier and write the next round.

Format each question the way the output file asks, and word it so "yes" accepts your recommended answer.

Each round the writer answers reshapes the tree: settled decisions push the frontier outward and unblock questions that depended on them. A question whose answer depends on another question still open in this round belongs to a _later_ round, not this one.

Finding _facts_ is your job, never the writer's. When a frontier question needs a fact from the environment (filesystem, tools, docs), look it up yourself with the read-only tools you have. The _decisions_ are the writer's: put each to them in the round.

The session is done when the frontier is empty: every branch of the design tree visited, nothing left silently assumed. Then the round is a final one, per the run's prompt, and the writer's "yes" is the gate.
