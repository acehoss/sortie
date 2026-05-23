---
status: accepted
date: 2026-05-23
decision-makers: TBD
---

# Inject Tracker Comments Into Running Agents Between Turns

## Context and Problem Statement

Sortie's orchestrator currently treats issue comments as static input. They
are fetched once, when the orchestrator calls `FetchIssueByID` at dispatch
time, and rendered into the turn-1 prompt as part of `issue.Comments`. Once
a worker has started, new comments authored by humans, by collaborating
agents, or by other automation are invisible to the running agent. The
operator's only recourse is to wait for the current run to finish and rely
on the next dispatch — which only happens if the issue remains in an
active state and the next poll tick reschedules it — to deliver the new
context.

Two adjacent feedback paths already exist, but neither solves the
in-flight comment problem:

- The reaction pipeline (`internal/orchestrator/ci_reconcile.go`,
  `internal/orchestrator/review_reconcile.go`) detects CI failures and PR
  review comments between worker runs and schedules a continuation
  dispatch carrying a structured `ContinuationContext`. By design, these
  reactions trigger a *new* run; they do not communicate with the active
  worker subprocess.
- The agent adapter contract (`internal/domain/agent.go`) executes one
  turn at a time via `RunTurn`, with events flowing back through a
  callback. There is no input channel kept open across turns; the worker
  re-invokes `RunTurn` with a fresh prompt for each iteration of the
  turn loop in `internal/orchestrator/worker.go`.

Operators want a third path: when a collaborator comments on an issue
that an agent is currently working on, the agent should see that comment
on its next turn rather than at the start of its next *run*. This enables
lightweight back-and-forth steering between humans (or other agents) and
the working agent without the overhead of stopping and restarting a
session, without the cost of a full re-fetch of issue context, and
without modifying the agent adapter contract.

The decision must address four coupled questions:

1. **Where the configuration lives** — whether comment steering reuses
   the `reactions.*` family or gets its own front-matter section.
2. **How comments are delivered to the agent** — as a continuation
   dispatch (matching reactions), as in-band template variables on the
   next turn's prompt (matching the existing per-turn render loop), or
   via a new mid-turn input channel (which would require an adapter
   contract change).
3. **How echo and feedback loops are prevented** — sortie itself posts
   comments on issues (handoff transitions, CI failure summaries, review
   responses), and an unfiltered loop would re-inject those comments to
   the agent on the very next turn.
4. **How the watermark survives the worker lifecycle** — whether the
   set of "already-seen" comment IDs is kept in memory per run, persisted
   per claim, or recomputed each turn.

## Decision Drivers

1. **Additive only.** Existing workflows without explicit opt-in must
   behave byte-for-byte identically. No mandatory schema changes, no new
   API calls per turn for workflows that do not need this feature.
2. **No adapter contract change.** Modifying `domain.AgentAdapter.RunTurn`
   to accept mid-turn input affects every adapter (Claude Code, Codex,
   Copilot) and forces each one to implement an async input pump. The
   benefit does not justify the surface-area expansion.
3. **Match the existing per-turn render loop.** The worker already
   re-renders the prompt template for every turn
   (`internal/orchestrator/worker.go:631-643`). Delivering new comments
   as a template variable on the next turn fits this loop with one extra
   call, one extra option, and no new control flow.
4. **Echo safety.** Sortie's own writes to the tracker
   (`domain.TrackerAdapter.CommentIssue`) and any agent-tool-driven
   writes must be detectable and excluded from steering. A reflection
   loop where the agent reads its own handoff comment and responds to it
   would be a correctness bug.
5. **Latency budget.** A user expectation of "comments arrive within
   ~one turn" is achievable with between-turn polling; sub-turn latency
   is not, and is not requested. The poll cost is one
   `FetchIssueComments` call per turn per running issue — small relative
   to the cost of the turn itself.
6. **Layer hygiene.** Comment fetching is already exposed on the
   tracker adapter interface (`FetchIssueComments`,
   `internal/domain/tracker.go:53-59`). The orchestrator may call it
   without any new adapter method. Prompt assembly is already in
   `internal/prompt/prompt.go`. No new package boundary is required.
7. **Template-engine strictness (ADR-0005).** Prompt templates render
   with `Option("missingkey=error")`. Any new template variable must be
   wired with a default-empty value so existing `WORKFLOW.md` files that
   do not reference it continue to render. New variables that the
   workflow author chooses to reference render normally.
8. **Session continuity.** This feature does not interact with
   `ResumeSessionID` or the reaction continuation pipeline. A new
   comment that arrives during a run is delivered on the next turn of
   the *same* session; if the run ends before delivery, the comment will
   appear in the next run's static `issue.Comments` via the existing
   pre-dispatch `FetchIssueByID` call.
9. **Failure isolation.** A transient tracker error during the
   between-turn comment fetch must not abort the worker. It is an
   advisory poll; the agent continues without the new comments and the
   next turn retries.
10. **Cost containment in test fixtures.** Integration tests for the
    reaction pipeline (`SORTIE_*_TEST=1` gates) already exercise
    `FetchIssueComments`. The new feature reuses that adapter method
    rather than introducing a new one, so no fixture or recorded-response
    infrastructure needs to be added.

## Considered Options

- **Option A.** Between-turn comment polling inside the worker turn
  loop. New comments since the last seen watermark are rendered into
  the next turn's prompt via a new template variable `.new_comments`.
  Opt-in via a new `steering.issue_comments` front-matter section.
- **Option B.** Treat new comments as a reaction. Add
  `reactions.issue_comments` and trigger a continuation dispatch when
  comments are detected. New comments are delivered as
  `ContinuationContext` on a fresh run, identical to the CI-failure and
  review-comment paths.
- **Option C.** Open a persistent stdin channel to the agent
  subprocess and stream comments mid-turn. Extend
  `domain.AgentAdapter` with a `SendUserMessage(ctx, sessionID, text)`
  method that each adapter implements.
- **Option D.** Have the agent itself poll for comments via an MCP
  tool exposed by sortie. The orchestrator does no polling; the agent
  decides when to fetch new comments by calling the tool between its
  own actions.

## Decision Outcome

Chosen option: **Option A (between-turn polling, in-band template
variable, opt-in via `steering.issue_comments`)**, because it is the
only option that delivers near-turn latency without changing the agent
adapter contract, without adding a second dispatch type to the
orchestrator state machine, and without giving operators a new
authoring surface beyond a single front-matter block.

### Wire format

```yaml
steering:
  issue_comments:
    enabled: true                         # default false
    author_filter:                        # optional; usernames to ignore
      - sortie-bot
      - github-actions[bot]
    self_marker: "<!-- sortie:no-steer -->"  # optional; default shown
```

All three sub-keys are optional. With `enabled: false` (the default,
including when the entire `steering` section is absent) the feature is
disabled and the worker turn loop does not call `FetchIssueComments`
between turns. This preserves the additive-only invariant for every
existing workflow.

The `steering` key is added to `knownTopLevelKeys` in
`internal/config/config.go` and gains a `SectionSchema` entry in
`internal/config/schema.go` with `AllowAdapterPassthrough: false` and
`AllowDynamicKeys: false`. The inner schema is fixed: only
`issue_comments` is recognized in v1. A separate dynamic-keys design
for future steering sources is out of scope.

### Why a new section, not `reactions.issue_comments`

Comment steering is operationally distinct from a reaction. A reaction
schedules a fresh dispatch after the current run completes and carries
structured failure context into the *first turn* of the next run.
Comment steering injects free-form text into the *next turn of the
current run*. They share neither the trigger semantics, the lifecycle,
nor the prompt-injection mechanism. Placing comment steering under
`reactions.*` would force operators to learn that two sibling keys
behave fundamentally differently — one starts a new run, the other
does not. A separate section makes the lifecycle distinction visible at
the configuration layer.

### Worker turn-loop integration

The worker, on entering its turn loop in `internal/orchestrator/
worker.go`, seeds a per-run `seenCommentIDs` set from the IDs of every
comment present on the dispatched `issue.Comments` slice. After each
successful turn, immediately after the existing
`FetchIssueStatesByIDs` call near `worker.go:790`, the worker calls
`FetchIssueComments(ctx, issue.ID)` when `steering.issue_comments.
enabled` is true.

The returned comments are filtered in three steps:

1. Drop any comment whose `ID` is in `seenCommentIDs`.
2. Drop any comment whose `Author` matches a member of
   `steering.issue_comments.author_filter` (case-insensitive exact).
3. Drop any comment whose `Body` contains the configured `self_marker`
   substring.

Surviving comments are added to `seenCommentIDs` and stored on the
worker's local turn-context for the next iteration. The next call to
`prompt.BuildTurnPrompt` receives them via a new render option
`prompt.WithNewComments([]domain.Comment)`. The template-data field
`.new_comments` is always populated (with an empty slice when there
are none) so `Option("missingkey=error")` does not fire on workflows
that reference the variable.

When `FetchIssueComments` returns an error, the worker logs WARN with
the comment-fetch error category and proceeds to the next turn with no
new comments. The error never propagates to `WorkerResult`; this is an
advisory poll, not a load-bearing call.

### Echo prevention

The orchestrator already writes to the tracker through
`CommentIssue` for handoff (ADR-0007), CI-failure escalation, and
review-failure escalation. Without protection, a comment-steering loop
would surface these writes to the agent on the next turn.

Two complementary defenses are applied:

- **Author filter (operator-configured).** Operators name the bot
  identities they want excluded. Most deployments use a single bot
  account for sortie's writes and can name it once.
- **Marker comment (orchestrator-applied).** Every `CommentIssue` call
  emitted by the orchestrator (handoff, escalation, future) is wrapped
  to append the configured marker substring on a final line. The
  default marker is `<!-- sortie:no-steer -->`, chosen because Jira
  ADF, GitHub-flavored Markdown, and Linear all render HTML comments
  as invisible. The marker is stripped from neither the human view
  nor the agent's static `issue.Comments` snapshot at dispatch time;
  it only suppresses re-injection into the running session that
  authored the action. The wrapper is a one-line helper in
  `internal/issuekit/` and does not change adapter implementations.

Both defenses are advisory. An operator who clears `author_filter` and
changes `self_marker` to an empty string disables them. Documentation
warns against this.

### Watermark lifecycle

`seenCommentIDs` is in-memory per worker run. It is seeded at run
start from `issue.Comments` and grown each turn. It is not persisted
to SQLite, not exposed on `RunningEntry`, and not surfaced in the
status endpoint.

Rationale: if the orchestrator restarts mid-run, the run is dead. The
next dispatch fetches the full comment list via the existing
`FetchIssueByID` call and renders it into the new turn-1 prompt as
part of `issue.Comments`, which is the existing static behavior. There
is no missed-comment window to close because the next run starts from
a fresh snapshot anyway.

### Interaction with existing front-matter fields

| Existing concern                | Interaction                                                                                                                                                                                     |
| ------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.max_turns`               | Unchanged. Comment steering does not add turns; it adds content to turns that already happen.                                                                                                   |
| `reactions.ci_failure`          | Independent. CI reactions schedule a new run; comment steering augments the active run. Both can be enabled together.                                                                           |
| `reactions.review_comments`     | Independent. PR review threads come through the SCM adapter on a separate cadence and dispatch path. Comment steering reads tracker-native comments only.                                       |
| `self_review.*`                 | Independent. Self-review runs after the coding turn loop; new comments arriving during self-review are not delivered until a future run (the self-review prompt is a separate template).        |
| Dynamic reload (Section 6.2)    | `steering.issue_comments.enabled` changes apply to future runs only. In-flight runs continue with the value they started with.                                                                  |
| Handoff comments (ADR-0007)     | Wrapped with the self-marker. Operators who already filter their bot user by author do not strictly need the marker but get it for free.                                                        |
| Workspace safety (Section 9.6)  | No interaction. Comment fetching is read-only and goes through the tracker adapter, not the workspace.                                                                                          |

### Non-goals

- **No mid-turn interruption.** A comment that arrives while a turn is
  running is delivered after the turn completes, not by canceling the
  in-flight `RunTurn` call. This is the deliberate trade-off that keeps
  the agent adapter contract unchanged.
- **No persistent watermark.** Survival across orchestrator restarts is
  not required because the next run's static comment snapshot covers
  the gap.
- **No de-duplication across runs.** Comments seen in a previous run
  and re-fetched as `issue.Comments` in the next run remain in the
  static snapshot. This is the existing behavior and is unchanged.
- **No per-comment routing.** Comments are not parsed for commands,
  mentions, or slash-prefixes by the orchestrator. The agent decides
  what to do with the text it receives.
- **No comment-driven dispatch.** A new comment on an issue that is
  not currently being worked does nothing special; eligibility, state,
  and capacity gates are unchanged.

### Considered Options in Detail

**Option B — Reaction-style continuation dispatch.** Comment steering
could be implemented as `reactions.issue_comments`, with the
reconciler detecting new comments between runs and scheduling a
continuation. The shape mirrors CI and review reactions exactly. It
was rejected for two reasons. First, latency: a continuation dispatch
requires the current run to complete (or to be canceled), the worker
to be released, the reconciler to detect the new comment on its next
tick, and a new worker to spin up and re-render the full turn-1
prompt. Multi-minute latency is normal for that path. Operators using
comments for live steering would find this unusably slow. Second,
semantic mismatch: reactions exist to recover from failures (CI broke,
review requested changes). Comments are neutral additions to the
issue's context, not failure signals. Reusing the reaction lifecycle
would conflate two different operational concepts and would force
escalation/max-retries machinery onto a flow that does not need it.

**Option C — Mid-turn stdin channel.** Extending `domain.AgentAdapter`
with `SendUserMessage(ctx, sessionID, text)` would deliver sub-turn
latency. It was rejected for three reasons. First, contract surface:
every adapter (Claude Code, Codex, Copilot, plus any future addition)
would need an implementation, and not all back-ends support
out-of-band input cleanly. Claude Code's CLI does not have a stable
mid-turn input interface; Codex's JSON-RPC stdin is structured around
tool replies, not free-form user messages; Copilot operates over an
HTTP session API where in-flight injection requires session-resume
semantics that vary by version. Second, semantics: an adapter that
accepts mid-turn input must define what happens to the in-flight turn
(interrupt? queue? merge?), and the answer differs per back-end —
which leaks back into the orchestrator as adapter-specific behavior.
Third, the user need does not justify it: between-turn latency
(seconds to a few minutes, bounded by turn duration) is acceptable for
the comment-steering use case, where the human is already
communicating asynchronously.

**Option D — Agent-driven polling via MCP tool.** Sortie could expose
a `tracker__fetch_new_comments` MCP tool and let the agent decide when
to call it. The orchestrator would do no polling. This was rejected
because it pushes coordination concerns into the prompt: the workflow
author must remember to instruct the agent to poll, and the agent's
attention budget is consumed by polling decisions. Worse, it removes
the orchestrator's ability to apply echo protection (the agent sees
raw comments and the orchestrator cannot filter its own writes
because the call originates inside the agent's session). The
between-turn pattern keeps coordination where it belongs.

## Consequences

### Positive

- **Near-turn-latency steering.** New comments reach the agent on the
  next turn rather than the next run.
- **Zero adapter changes.** Claude Code, Codex, and Copilot adapters
  are untouched. The feature is delivered entirely in orchestrator,
  prompt, and config code.
- **Zero migration cost.** No persistence schema change. No
  `RunningEntry` field additions. In-flight rows from a pre-feature
  binary continue to run unchanged.
- **Predictable opt-in.** Workflows without `steering.issue_comments.
  enabled: true` make zero new API calls per turn. Operators choose
  the cost.
- **Echo-safe by default.** The self-marker plus the author filter
  cover the obvious feedback-loop cases without operator effort. The
  marker is invisible in tracker UIs.
- **Reuses existing adapter method.** `FetchIssueComments` is already
  on `domain.TrackerAdapter` and exercised by reaction tests, so the
  new code path inherits coverage and recorded-response fixtures.

### Negative

- **One extra adapter call per turn per running issue.** Cheap on
  Jira and GitHub but non-zero. Operators with very short turns and
  many concurrent agents should be aware. A future additive
  enhancement could share the existing `FetchIssueStatesByIDs` poll
  if the adapter is extended to return comments in the same call.
- **Self-marker leaks into raw comment bodies.** The marker is an
  HTML comment and invisible in every supported tracker UI today, but
  any future tracker that does not strip HTML comments would surface
  it. The marker is configurable so operators can adapt; the default
  is conservative.
- **Author filter is per-workflow, not per-tracker.** Operators with
  multiple bot identities (handoff bot, CI bot, review bot) list all
  of them. This is verbose but explicit. A future config layer could
  derive the filter from `tracker.bot_user` if such a field is ever
  added.
- **Between-turn polling does not see comments authored after the
  final turn.** Comments arriving after the last turn of a run are
  not delivered to that run. They will appear in the next run's
  static snapshot, which matches today's behavior.
- **No metric for steering-comment delivery in v1.** The status
  endpoint does not expose a count. Operators rely on agent transcripts
  to confirm delivery. Adding a metric is an additive follow-up.

## Confirmation

The decision is validated when all of the following are true after
implementation:

1. **Backward compatibility.** Every existing example workflow under
   `examples/` and every fixture under `internal/workflow/testdata/`
   continues to parse and run with no observable behavior change.
   Workflows without `steering.issue_comments` make zero new
   `FetchIssueComments` calls.
2. **Opt-in delivery.** A unit test in
   `internal/orchestrator/worker_test.go` exercises a workflow with
   `steering.issue_comments.enabled: true`, drives a fake tracker
   that returns a new comment between turns, and asserts the comment
   appears in the rendered prompt for the following turn.
3. **Echo prevention.** Table-driven tests in the same file cover the
   author filter (case-insensitive exact match), the self-marker
   substring filter, and the seeded watermark (comments present at
   dispatch are not re-delivered).
4. **Failure isolation.** A test injects a transient
   `FetchIssueComments` error and asserts the worker continues to the
   next turn with no new comments and the run completes normally.
5. **Schema validation.** Unit tests in
   `internal/config/schema_test.go` cover the warning case (unknown
   sub-keys under `steering` and `steering.issue_comments`) and the
   error cases (non-boolean `enabled`, non-list `author_filter`,
   non-string `self_marker`).
6. **Self-marker integration.** A test asserts that every
   `CommentIssue` call from the orchestrator (handoff, escalation)
   includes the configured marker substring on the final line.
7. **Documentation.** `docs/workflow-reference.md` describes the
   `steering` section and the `.new_comments` template variable.
   `docs/architecture.md` Section 10 (Agent Adapter Contract) and
   Section 12 (Prompt Construction) are updated to note the new
   between-turn context source. The architecture digest gains a
   one-line note pointing at the new section.
