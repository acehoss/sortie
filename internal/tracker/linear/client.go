package linear

import "context"

// Client is the minimal GraphQL surface the Linear adapter depends on.
// Each method maps to one Linear GraphQL operation; the implementation
// is responsible for query construction, variable binding, response
// parsing, and error classification into *domain.TrackerError.
//
// Implementations must be safe for concurrent use by the orchestrator's
// poll loop and reconciliation goroutine.
//
// All errors should be normalized to *domain.TrackerError before being
// returned. Context cancellation or deadline expiration may surface as
// ctx.Err() directly per the TrackerAdapter contract.
type Client interface {
	// QueryIssues runs the issues(...) connection query with the given
	// filter and pagination cursor. The caller pages by re-calling with
	// PageInfo.EndCursor as after until HasNextPage is false. A nil or
	// empty after string requests the first page.
	//
	// Returned issues populate Labels, InverseRelations, and slim
	// nested fields; Comments is nil (not fetched here — use
	// QueryIssueByKey or QueryIssueComments).
	QueryIssues(ctx context.Context, filter IssuesFilter, first int, after string) (*IssueConnection, error)

	// QueryIssueByKey fetches a single issue by UUID or human
	// identifier (e.g. "ENG-123"). Linear's Query.issue(id) accepts
	// either form. The returned Issue has Comments populated with the
	// first page (up to 50); callers needing more pages use
	// QueryIssueComments.
	//
	// Returns ErrTrackerNotFound when the issue does not exist.
	QueryIssueByKey(ctx context.Context, key string) (*Issue, error)

	// QueryIssueStatesByKeys batches lightweight state lookups via
	// GraphQL aliasing. The implementation chunks internally to keep
	// each request under Linear's single-query complexity cap. Keys
	// may be UUIDs or human identifiers; the returned map is keyed by
	// the input string. Issues that do not exist are omitted.
	QueryIssueStatesByKeys(ctx context.Context, keys []string) (map[string]string, error)

	// QueryIssueComments pages comments for the given issue. Callers
	// using QueryIssueByKey already have the first 50; this method is
	// used only when more pages exist or when refetching comments
	// without the full issue payload.
	QueryIssueComments(ctx context.Context, issueID string, first int, after string) (*CommentConnection, error)

	// QueryStateIDByName resolves a workflow state name to its UUID
	// in the team that owns the given issue. The single query walks
	// issue → team → states(filter: name) so neither a per-team
	// workflow-state cache nor a separate team-UUID lookup is needed.
	//
	// Name comparison is case-insensitive (eqIgnoreCase). Returns an
	// ErrTrackerPayload TrackerError when no state with that name
	// exists in the issue's team. Returns ErrTrackerNotFound when the
	// issue itself does not exist.
	QueryStateIDByName(ctx context.Context, issueID string, stateName string) (stateID string, err error)

	// QueryIssueLabels returns the team UUID and the issue's current
	// label set in a single round-trip. Used by AddLabel to (a)
	// resolve team scope for downstream label-create or team-labels
	// cache refresh, (b) check whether the label is already attached,
	// and (c) build the full labelIds array for the subsequent
	// issueUpdate (which replaces, not appends).
	//
	// Returns ErrTrackerNotFound when the issue does not exist.
	QueryIssueLabels(ctx context.Context, issueID string) (*IssueLabelsResult, error)

	// QueryTeamLabels returns all labels defined for the given team.
	// Used to refresh the adapter's per-team label cache on miss or
	// after a concurrent label-create race.
	QueryTeamLabels(ctx context.Context, teamID string) ([]Label, error)

	// MutationIssueUpdateState transitions an issue to the given
	// workflow state UUID via issueUpdate.
	MutationIssueUpdateState(ctx context.Context, issueID string, stateID string) error

	// MutationIssueUpdateLabels replaces the issue's label set with
	// the given UUIDs. To add a label, callers MUST include all
	// existing label UUIDs alongside the new one — Linear's
	// issueUpdate.labelIds replaces rather than appends.
	MutationIssueUpdateLabels(ctx context.Context, issueID string, labelIDs []string) error

	// MutationCommentCreate posts a markdown comment and returns the
	// created comment's UUID.
	MutationCommentCreate(ctx context.Context, issueID string, body string) (commentID string, err error)

	// MutationIssueLabelCreate creates a new team-scoped label and
	// returns its UUID. On uniqueness conflict (concurrent create
	// race), implementations return a *domain.TrackerError with kind
	// ErrTrackerPayload and a message containing the substring
	// "already exists" — the adapter retries via QueryTeamLabels.
	MutationIssueLabelCreate(ctx context.Context, teamID string, name string) (labelID string, err error)
}

// IssuesFilter is the Sortie-shaped filter for QueryIssues. The
// implementation translates this into Linear's IssueFilter GraphQL
// input.
//
// At least one of TeamKey or TeamID must be set; otherwise the query
// would span the entire workspace. The adapter always sets TeamKey
// from the configured tracker.project.
type IssuesFilter struct {
	// TeamKey scopes the query by Team.key (e.g. "ENG"). Required.
	TeamKey string

	// StateNames restricts results to issues whose state.name is in
	// the list. Nil or empty means no state filter.
	StateNames []string
}
