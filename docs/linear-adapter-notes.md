# Linear GraphQL API: Adapter Research Notes

> Linear public GraphQL API, researched May 2026.
> Reference for implementing the Linear `TrackerAdapter`.

---

## Authentication

Linear supports two authentication methods. Sortie uses Personal API Keys.

### Personal API Keys (recommended for Sortie)

The standard method for scripts and service integrations. Generated from the user's Linear settings.

- Generate at `https://linear.app/settings/account/security`.
- Header: `Authorization: <API_KEY>` — **note: no `Bearer` prefix**.
- API keys are user-scoped, static, do not expire automatically, and inherit the user's workspace permissions.

This is the recommended method for Sortie because it runs as a headless background service.

### OAuth 2.0

For applications acting on behalf of multiple users. Token format is `Authorization: Bearer <ACCESS_TOKEN>` (with `Bearer`). Not used by Sortie — the orchestrator runs as a single principal and does not need a 3-legged auth flow.

### Config mapping

| Config field       | Value                                                           |
| ------------------ | --------------------------------------------------------------- |
| `tracker.endpoint` | Optional. Defaults to `https://api.linear.app/graphql`.         |
| `tracker.api_key`  | The personal API key (e.g. `lin_api_...`). Sent verbatim.       |
| `tracker.project`  | The Linear team key, e.g. `ENG`. Used to scope issue queries.   |

Optional adapter config (read from the workflow's `extensions.linear` object):

| Key                | Default                                                          | Purpose                                          |
| ------------------ | ---------------------------------------------------------------- | ------------------------------------------------ |
| `active_states`    | `["Backlog", "Todo"]`                                            | Names of states treated as candidate-eligible.   |
| `assignee`         | empty (no assignee filter)                                       | Linear user UUID, or literal `"me"` to resolve via the viewer query at adapter construction. Restricts both `FetchCandidateIssues` and `FetchIssuesByStates` to issues assigned to that user. |
| `query_filter`     | empty                                                            | Reserved for an extra `IssueFilter` JSON object. |

The team key (project) does not need to be a UUID — Linear's `TeamFilter` accepts `key: { eq: "ENG" }` directly. The adapter caches the resolved team UUID after the first query for use in label and workflow-state lookups.

---

## Endpoint and transport

- Single endpoint: `POST https://api.linear.app/graphql`
- Request: `Content-Type: application/json`, body `{ "query": "...", "variables": {...} }`
- Response: always `application/json`. Successful responses have `{ "data": {...} }`. Errors live in `{ "errors": [...] }` (potentially alongside `data`).
- Network timeout: `30000 ms` (matches §11.2 default).
- HTTP status semantics differ from REST APIs:
  - `200 OK` — request was accepted by the gateway. **The body may still contain `errors`.** Always inspect.
  - `400 Bad Request` — used for rate-limit responses (`extensions.code == "RATELIMITED"`) and malformed queries. Inspect the body.
  - `401 Unauthorized` — invalid or revoked API key.
  - `5xx` — server-side failure.

### Standard error envelope

```json
{
  "errors": [
    {
      "message": "Entity not found",
      "path": ["issue"],
      "extensions": {
        "code": "FEATURE_NOT_ACCESSIBLE",
        "type": "InvalidInput",
        "userError": true
      }
    }
  ],
  "data": null
}
```

Common `extensions.code` values:

| Code                    | Meaning                                                          | Maps to                  |
| ----------------------- | ---------------------------------------------------------------- | ------------------------ |
| `RATELIMITED`           | Request denied due to rate or complexity limits.                 | `ErrTrackerAPI`          |
| `INVALID_INPUT`         | Input failed validation (bad UUID, missing required field).      | `ErrTrackerPayload`      |
| `AUTHENTICATION_ERROR`  | Token missing or invalid.                                        | `ErrTrackerAuth`         |
| `FORBIDDEN`             | Token valid but lacks scope for the requested operation.         | `ErrTrackerAuth`         |
| `FEATURE_NOT_ACCESSIBLE`| Workspace plan does not include the feature.                     | `ErrTrackerAPI`          |
| `INTERNAL_SERVER_ERROR` | Server-side failure.                                             | `ErrTrackerTransport`    |
| `ENTITY_NOT_FOUND`      | Requested resource (issue, label, team) does not exist.          | `ErrTrackerNotFound`     |

Partial success: GraphQL can return `data` populated and `errors` non-empty for the same response. The adapter treats any non-empty `errors` array on a write mutation as failure. For read queries, the adapter inspects whether the requested data field is null; if data is usable, the errors are logged at debug level and the partial result is returned.

---

## Rate limits

Linear enforces three independent budgets, all keyed by authenticated user (API key) or IP (unauthenticated):

| Budget                  | API key    | OAuth      | Unauth   | Window |
| ----------------------- | ---------- | ---------- | -------- | ------ |
| Requests                | 5,000      | 5,000      | 600      | 1 hour |
| Complexity              | 3,000,000  | 2,000,000  | 100,000  | 1 hour |
| Single-query complexity | 10,000     | 10,000     | 10,000   | n/a    |

Each response includes headers:

- `X-RateLimit-Requests-Limit`, `X-RateLimit-Requests-Remaining`, `X-RateLimit-Requests-Reset` (epoch ms).
- `X-Complexity`, `X-RateLimit-Complexity-Limit`, `X-RateLimit-Complexity-Remaining`, `X-RateLimit-Complexity-Reset`.
- Some endpoints add `X-RateLimit-Endpoint-*` for per-endpoint sub-budgets.

When a limit is exceeded, the response is HTTP 400 with the `RATELIMITED` extension code. The adapter logs the reset epoch at WARN and returns `ErrTrackerAPI`. The orchestrator's existing retry semantics handle the backoff at the candidate-fetch level.

### Complexity scoring

- Each scalar field: 0.1 points.
- Each object: 1 point.
- Each connection (`issues`, `comments`, `labels`, ...) multiplies the child cost by the requested page size (default 50). Cap explicitly via `first: N` whenever the call needs fewer.
- Final value rounded up to nearest integer.

Examples:

- `issue(id) { id title state { name } }` ≈ 1 (issue) + 0.1 (id) + 0.1 (title) + 1 (state object) + 0.1 (name) = **3**.
- `issues(first: 50, filter: {...}) { nodes { id identifier title state { name } } }` ≈ 50 × (1 + 0.4) + 50 × 1 (state objects) = **120**.

Practical implication: always pass `first:` explicitly and request only the fields needed for the call. The orchestrator's poll cadence (default 60 s) keeps us well under the 5,000 req/hr budget at expected scale (≤ 20 active issues).

---

## Pagination

Linear uses Relay-style cursor pagination. Responses include:

```graphql
pageInfo {
  hasNextPage
  endCursor
}
```

The adapter pages by passing `after: $endCursor` and `first: 50` until `hasNextPage` is false. The minimum page size is 1; the maximum is 250. Default page size for Sortie: **50** (matches §11.2).

Sort order: results are sorted by `createdAt` ASC by default. The adapter explicitly orders candidate fetches by `priority` (where supported via filter ordering) and accepts the default `createdAt` ordering for terminal-cleanup queries.

If `pageInfo.hasNextPage` is true but `endCursor` is empty/null, return `ErrTrackerPayload` with kind `tracker_missing_end_cursor` (§11.4).

---

## Operations

Each `TrackerAdapter` method maps to one or more GraphQL operations.

### 1. `FetchCandidateIssues` — `issues` query, filtered by team + active states

```graphql
query CandidateIssues($teamKey: String!, $states: [String!]!, $first: Int!, $after: String) {
  issues(
    first: $first
    after: $after
    filter: {
      team:  { key:  { eq: $teamKey } }
      state: { name: { in: $states } }
    }
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      branchName
      url
      createdAt
      updatedAt
      state { name }
      assignee { displayName name email }
      parent { id identifier }
      labels(first: 50) { nodes { name } }
      inverseRelations(first: 50) {
        nodes {
          type
          issue { id identifier state { name } }
        }
      }
    }
    pageInfo { hasNextPage endCursor }
  }
}
```

Variables: `{ teamKey: "ENG", states: ["Backlog", "Todo"], first: 50, after: null|<cursor> }`.

Comments are **not** fetched in this query (cost reduction). The candidate result populates `Issue.Comments = nil`. The orchestrator promotes the candidate via `FetchIssueByID` before dispatch, which does include comments.

`assignee.displayName` is the user's chosen display name; fall back to `name` then `email`. If `assignee` is null, populate the domain field as empty string.

`inverseRelations` with `type == "blocks"` are the issues that block this one (the source side of the relation). The blocker is `IssueRelation.issue`.

### 2. `FetchIssueByID` — `issue` query, accepting UUID or human identifier

```graphql
query IssueByID($id: String!) {
  issue(id: $id) {
    id
    identifier
    title
    description
    priority
    branchName
    url
    createdAt
    updatedAt
    state { name }
    assignee { displayName name email }
    parent { id identifier }
    labels(first: 50) { nodes { name } }
    inverseRelations(first: 50) {
      nodes {
        type
        issue { id identifier state { name } }
      }
    }
    comments(first: 50, orderBy: createdAt) {
      nodes { id body createdAt user { displayName name email } }
      pageInfo { hasNextPage endCursor }
    }
  }
}
```

`Query.issue(id: String!)` accepts **either** a UUID **or** a human identifier (`"ENG-123"`). Sortie passes whichever value it has.

If comments span multiple pages, the adapter pages over `issueComments` (see operation 6) and appends. The first 50 inline is enough for most issues.

On not-found, Linear returns `errors: [{ extensions: { code: "ENTITY_NOT_FOUND" } }]` with `data: { issue: null }`. The adapter maps this to `ErrTrackerNotFound`.

### 3. `FetchIssuesByStates` — `issues` query, filtered by team + arbitrary states

Same query shape as `FetchCandidateIssues`, but called with the orchestrator-supplied state list (e.g. terminal states like `["Done", "Canceled"]`). Pages through all results.

### 4. `FetchIssueStatesByIDs` — batched `issue` queries via aliasing

```graphql
query IssueStatesByIDs($id_0: String!, $id_1: String!) {
  i0: issue(id: $id_0) { id state { name } }
  i1: issue(id: $id_1) { id state { name } }
}
```

The adapter builds the query string and variable map dynamically from the input list. To keep complexity well below the 10,000-per-query cap, the adapter chunks at **50 IDs per request**. At ~2.2 points per `issue` block, 50 IDs costs ~110 — comfortable headroom.

Missing IDs surface as either (a) GraphQL `errors` array entries with `path: ["i7"]` and code `ENTITY_NOT_FOUND`, or (b) `data.i7 == null`. Either way the adapter omits them from the result map (per §11.1 — "issues not found are omitted").

### 5. `FetchIssueStatesByIdentifiers` — same shape as `FetchIssueStatesByIDs`

`Query.issue(id)` accepts human identifiers, so the same aliased-batch pattern works. The result map is keyed by the identifier the caller passed in.

### 6. `FetchIssueComments` — `issue.comments` connection

```graphql
query IssueComments($id: String!, $first: Int!, $after: String) {
  issue(id: $id) {
    comments(first: $first, after: $after, orderBy: createdAt) {
      nodes {
        id
        body
        createdAt
        user { displayName name email }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}
```

Returns `[]domain.Comment` ordered by creation time ASC. Empty non-nil slice when no comments exist. Not-found maps to `ErrTrackerNotFound`.

The `user` field is null for bot/integration comments; normalize to empty author string.

### 7. `TransitionIssue` — one resolve query + one mutation

Linear requires a workflow-state **UUID**, not a name. The flow walks `issue → team → states(filter)` in a single GraphQL request to resolve the UUID, then applies the update.

1. Resolve the state UUID with one query (no team-lookup hop, no per-team cache):
   ```graphql
   query ResolveStateID($issueId: String!, $stateName: String!) {
     issue(id: $issueId) {
       team {
         states(filter: { name: { eqIgnoreCase: $stateName } }, first: 1) {
           nodes { id }
         }
       }
     }
   }
   ```
   Case-insensitive match avoids casing mismatches between operator config ("in review") and Linear's stored name ("In Review"). Empty nodes array → `ErrTrackerPayload` with message `no workflow state named %q in issue's team`.

2. Apply:
   ```graphql
   mutation IssueUpdateState($id: String!, $stateId: String!) {
     issueUpdate(id: $id, input: { stateId: $stateId }) {
       success
       issue { id state { name } }
     }
   }
   ```
3. `success: false` with no GraphQL error → `ErrTrackerAPI` (defensive; Linear typically signals failures via `errors`).

Adapted from Symphony's `state_lookup_query` pattern. The embedded-query approach removes both the team→workflow-states cache and the issue→team cache that an unembedded design would need.

### 8. `CommentIssue` — `commentCreate` mutation

```graphql
mutation CommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id }
  }
}
```

The body is plain markdown — no wrapping required (contrast with Jira's ADF). Linear treats URLs to issues, users, and projects as mentions automatically.

### 9. `AddLabel` — lookup + optional create + `issueUpdate` with full label list

Linear has no atomic "attach label by name" mutation. Adding a label is a three-step operation. The adapter encapsulates this so the orchestrator's call site remains a single `AddLabel(issueID, name)` invocation.

**Step 1.** Fetch current labels on the issue (and team UUID, if not cached):
```graphql
query IssueLabels($id: String!) {
  issue(id: $id) {
    id
    team { id }
    labels(first: 50) { nodes { id name } }
  }
}
```

**Step 2.** Check the team-label cache for `(teamID, lowercase(name))`. On miss, refresh:
```graphql
query TeamLabels($teamId: String!) {
  team(id: $teamId) {
    labels(first: 100) { nodes { id name } }
  }
}
```

**Step 3.** If the label still does not exist, create it. **(Per project decision: create-on-miss is the intended behavior.)**
```graphql
mutation IssueLabelCreate($teamId: String!, $name: String!) {
  issueLabelCreate(input: { teamId: $teamId, name: $name }) {
    success
    issueLabel { id name }
  }
}
```

On uniqueness conflict (concurrent create race), the mutation returns an error with extension code `INVALID_INPUT`. The adapter re-queries `TeamLabels` once and uses the now-existing ID.

**Step 4.** Attach by sending the full label-ID array:
```graphql
mutation IssueAddLabel($id: String!, $labelIds: [String!]!) {
  issueUpdate(id: $id, input: { labelIds: $labelIds }) {
    success
    issue { id labels(first: 50) { nodes { name } } }
  }
}
```

The `labelIds` array MUST include all existing label UUIDs plus the new one — `issueUpdate.labelIds` replaces, not appends. Step 1 supplies the existing IDs.

**Permissions degradation.** If the API key lacks scope for `issueLabelCreate` (returns `FORBIDDEN`), the adapter returns `ErrTrackerAuth`. Per the §10.x non-fatal contract, the orchestrator logs WARN and continues. Operators are advised in the README that CI escalation requires a key with label-create scope; if the scope isn't available the team can pre-create the escalation label (`needs-human` by default) and the cached lookup will succeed.

---

## Normalization rules

Mapping from Linear GraphQL response shapes to `domain.Issue` (§11.3, §4.1.1).

| `domain.Issue` field | Linear source                                                 | Notes                                                                                  |
| -------------------- | ------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `ID`                 | `issue.id`                                                    | UUID, opaque.                                                                          |
| `Identifier`         | `issue.identifier`                                            | E.g. `"ENG-123"`.                                                                      |
| `DisplayID`          | `""` (empty)                                                  | Linear identifiers are already display-ready.                                          |
| `Title`              | `issue.title`                                                 |                                                                                        |
| `Description`        | `issue.description`                                           | Markdown. May be null → empty string.                                                  |
| `Priority`           | `int(issue.priority)` if 1–4, else `nil`                      | Linear: 0 = No priority, 1 = Urgent, 2 = High, 3 = Medium, 4 = Low. 0 maps to `nil`.   |
| `State`              | `issue.state.name`                                            | Original casing preserved; orchestrator lowercases for comparisons.                    |
| `BranchName`         | `issue.branchName`                                            | Linear-generated suggested branch.                                                     |
| `URL`                | `issue.url`                                                   |                                                                                        |
| `Labels`             | `[lowercase(label.name) for label in issue.labels.nodes]`     | Non-nil empty slice when none.                                                         |
| `Assignee`           | first non-empty of `displayName`, `name`, `email`; else `""`  |                                                                                        |
| `IssueType`          | `""` (empty)                                                  | Linear has no first-class issue-type field. Could surface a label-based heuristic later; punt for now. |
| `Parent`             | `{ID, Identifier}` from `issue.parent`; nil if absent         |                                                                                        |
| `Comments`           | nil when not fetched; `[]Comment` ordered by `createdAt`      | Empty non-nil when fetched-and-empty.                                                  |
| `BlockedBy`          | `inverseRelations.nodes` where `type == "blocks"` → `{issue.id, issue.identifier, issue.state.name}` | Non-nil empty when none.                |
| `CreatedAt`          | `issue.createdAt`                                             | ISO-8601, passed through.                                                              |
| `UpdatedAt`          | `issue.updatedAt`                                             | ISO-8601, passed through.                                                              |

### Comment normalization

| `domain.Comment` field | Linear source                                                |
| ---------------------- | ------------------------------------------------------------ |
| `ID`                   | `comment.id`                                                 |
| `Author`               | first non-empty of `user.displayName`, `user.name`, `user.email`; else `""` (bot/integration comments have null `user`) |
| `Body`                 | `comment.body` (markdown)                                    |
| `CreatedAt`            | `comment.createdAt`                                          |

---

## Quirks and gotchas

- **`Authorization` header has no `Bearer` for API keys.** A common bug; OAuth tokens *do* require `Bearer`. The adapter selects the prefix based on whether the configured key starts with `lin_api_` (API key) vs an OAuth access token.
- **Rate-limit errors return HTTP 400, not 429.** The adapter must inspect `errors[].extensions.code == "RATELIMITED"` rather than relying on status code.
- **`issueUpdate.labelIds` replaces, not appends.** Always fetch existing labels first when adding.
- **`Query.issue(id)` accepts UUIDs *and* human identifiers.** This simplifies state-refresh queries but means UUID-vs-identifier confusion is silent — Linear won't tell you which form was used.
- **Workflow state names are team-scoped.** A workspace can have multiple "In Progress" states, one per team. Always scope state lookups by `team.key` (filter) or `team.id` (label/state ID resolution).
- **`priority: 0` means "No priority"** in Linear's data model. The adapter maps 0 → `nil` to match domain semantics where lower-is-higher-priority assumes priority is set.
- **`branchName` is always present.** Linear auto-generates one (`ahoss/eng-123-foo-bar`). No null handling needed.
- **`assignee` is nullable.** Bot accounts and unassigned issues both yield `null`.
- **Connection page sizes default to 50, max 250.** Always specify `first:` to keep complexity costs predictable.
- **Archived resources are hidden by default.** No `includeArchived: true` needed — Sortie never wants archived issues.
- **`X-Complexity` header.** Useful for log/metric introspection (`sortie_tracker_complexity_total{tracker="linear"}` could be added later).
- **Mentions in comment bodies.** Linear converts plain URLs of the form `https://linear.app/<workspace>/issue/ENG-123` into mention pills on read. The adapter sends bodies verbatim; agents can include such URLs to produce mentions.
- **Mutation `success` field.** Most mutations return `{ success: Boolean! }`. The adapter treats `success == false` with no GraphQL errors as `ErrTrackerAPI`. In practice Linear always surfaces failures via `errors[]`.

---

## Authorization scope requirements

A Linear personal API key inherits the creator's workspace permissions. The Sortie service account needs:

| Operation                                | Required permission                                                |
| ---------------------------------------- | ------------------------------------------------------------------ |
| `FetchCandidateIssues` (read issues)     | Workspace member (read access to the configured team).             |
| `FetchIssueByID`, `FetchIssueComments`   | Same.                                                              |
| `TransitionIssue` (write `stateId`)      | Workspace member (write access to the configured team).            |
| `CommentIssue` (write comments)          | Workspace member.                                                  |
| `AddLabel` — attach existing             | Workspace member (write access).                                   |
| `AddLabel` — create missing label        | Workspace admin OR explicit "Manage labels" permission.            |

If the operator does not want to grant label-create scope, they pre-create the escalation label (default `needs-human`) inside Linear for the configured team. The adapter's cached lookup will then succeed without needing `issueLabelCreate`.

---

## Open questions and decisions made

1. **Q: Does `issueUpdate` trigger Linear webhooks even when called by the issue's owner?**
   - Assumed: yes. Webhooks fire on state transitions regardless of the triggering principal. (Linear's documented webhook flow doesn't carve out API-driven changes.) Will confirm during UAT.

2. **Q: Should we support workspace-level labels in addition to team-scoped?**
   - **Decision: no.** Linear allows workspace-level labels (no `teamId` in `issueLabelCreate`), but the simpler model — labels scoped to the issue's team — is sufficient for CI escalation. Workspace labels are also harder to validate against permissions degradation.

3. **Q: How do we map Linear's `priority: 0` (No priority) to dispatch ordering?**
   - **Decision: `nil`.** Matches `domain.Issue.Priority` semantics (nil → orchestrator-defined default sort position).

4. **Q: Should `FetchCandidateIssues` respect the configured `active_states` list, or always use Linear's built-in `state.type` enum (`backlog`, `unstarted`, `started`)?**
   - **Decision: respect `active_states` (by name).** Matches Jira/GitHub adapter behavior and lets operators configure the trigger more precisely.

5. **Q: Are there state-type vs state-name pitfalls?**
   - **Yes.** Linear's `WorkflowState.type` enum (`triage`, `backlog`, `unstarted`, `started`, `completed`, `canceled`) is workspace-immutable, but a team can have *multiple* states of the same type (e.g. "In Review" and "QA" both `started`). The adapter filters on `state.name` because that's what operators configure.

6. **Q: How do we handle concurrent `AddLabel` calls racing on `issueLabelCreate`?**
   - **Decision: retry once on `INVALID_INPUT` from `issueLabelCreate`, re-querying team labels.** A second loss means the next reconcile tick handles it.
