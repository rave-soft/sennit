---
name: junior-development-loop
description: Iterative development workflow where the main agent plans, delegates implementation to junior-developer, reviews the result, and repeats until correct. Use when implementing a feature, fix, refactor, or tests through a junior developer.
---

# Junior Development Loop

Act as the tech lead. You own investigation, planning, review, and final
verification. The `junior-developer` subagent owns implementation. Do not write
the implementation yourself unless the escalation rules require it.

## 1. Investigate And Plan

- Read `AGENTS.md` and all code relevant to the request before planning.
- Resolve ambiguities that materially affect behavior before delegation.
- Split the work into small, independently verifiable steps.
- Record the plan with `todowrite`. Keep exactly one step in progress.
- For every step define its goal, entry points, constraints, expected behavior,
  and required verification.

## 2. Delegate One Step

Launch `junior-developer` with the task tool and wait for its result. Delegate
only the current step, never the entire plan.

The prompt must be self-contained because the subagent starts with fresh
context. Include:

- the precise goal and acceptance criteria;
- relevant files, symbols, and surrounding architecture;
- project conventions and constraints that apply;
- behavior and files that must not change;
- tests, formatting, and checks it must run;
- an instruction to report changed files, results, assumptions, and risks.

Tell the subagent to implement the change, not merely propose code.

## 3. Review Independently

Do not accept the subagent's report as proof. Review the working tree yourself:

- inspect `git diff` and read every changed hunk in context;
- compare behavior with the step's acceptance criteria;
- check correctness, regressions, error handling, project conventions, and
  unintended scope expansion;
- verify that tests meaningfully cover changed behavior;
- run the relevant tests, formatter, build, and static checks yourself.

Do not modify unrelated user or agent changes in the worktree.

## 4. Return Findings And Repeat

If review finds defects, resume the same `junior-developer` task by passing its
`task_id` to the task tool. Do not start a fresh subagent for corrections.

Provide actionable findings ordered by severity. Every finding should include
the file and line or symbol, the observed problem, the required behavior, and
the verification that must pass. Ask the subagent to fix the findings and run
the focused checks, then repeat the independent review.

Continue until the step meets all acceptance criteria. Allow at most three
implementation and review rounds per step.

## 5. Escalate When Needed

After three failed rounds:

- fix a small, obvious residual issue yourself and disclose that in the final
  summary; or
- if the issue is substantial, stop implementation, revise or split the step,
  and report the blocker to the user rather than repeatedly guessing.

Never silently weaken acceptance criteria to complete a step.

## 6. Complete The Work

- Mark a step complete only after implementation and independent verification
  succeed.
- Move to the next step and repeat delegation and review.
- After the final step, run the broadest practical project test suite and
  inspect the complete diff.
- Summarize implemented behavior, important decisions, verification results,
  any senior-agent fixes, and remaining risks.
- Do not commit unless the user explicitly asks.
