// Package linear implements the Linear GraphQL tracker adapter.
//
// The package is split into three layers:
//
//   - [Client] (this file and client.go) — a minimal interface that
//     mirrors the Linear GraphQL operations Sortie depends on. Each
//     method corresponds to one GraphQL query or mutation. The real
//     implementation is a GraphQL transport over [httpkit.Client]; the
//     fake in subpackage linearmock implements the same interface for
//     tests.
//
//   - Wire types ([Issue], [Comment], [WorkflowState], ...) — Go shapes
//     that mirror Linear's GraphQL response objects field-for-field.
//     These are separate from [domain.Issue]; the adapter normalizes
//     wire types to domain types in normalize.go.
//
//   - Adapter (linear.go) — the [domain.TrackerAdapter] implementation
//     that orchestrates Client calls, holds caches (team workflow
//     states, team labels), and produces domain types.
package linear

// PageInfo is the standard Relay cursor pagination envelope returned by
// every Linear connection field.
type PageInfo struct {
	HasNextPage bool
	EndCursor   string
}

// User represents a Linear user as referenced by issues and comments.
// Linear returns nil for bot or integration actors.
type User struct {
	DisplayName string
	Name        string
	Email       string
}

// ParentRef is the slim parent reference returned on Issue.parent.
type ParentRef struct {
	ID         string
	Identifier string
}

// WorkflowState is a team-scoped issue state. Names are not unique
// across teams; the adapter scopes state lookups by team UUID.
//
// Type is Linear's built-in category enum
// ("triage", "backlog", "unstarted", "started", "completed", "canceled").
// A team may have multiple states sharing the same Type.
type WorkflowState struct {
	ID   string
	Name string
	Type string
}

// Label is a team-scoped issue label.
type Label struct {
	ID   string
	Name string
}

// IssueRelation represents one edge of an IssueRelation connection.
//
// Type is one of "blocks", "duplicate", or "related". To resolve the
// "blocked by" inverse for a given issue, query that issue's
// inverseRelations and keep nodes where Type == "blocks": the blocker
// is Issue (the source side of the relation).
type IssueRelation struct {
	Type  string
	Issue IssueRef
}

// IssueRef is a slim issue reference used inside relations and other
// nested contexts where the full Issue shape would inflate complexity.
type IssueRef struct {
	ID         string
	Identifier string
	State      string
}

// Issue mirrors Linear's GraphQL Issue type as fetched by Sortie.
// Only fields the adapter consumes are present; extending the
// selection set requires a corresponding field here.
type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	Priority    float64
	BranchName  string
	URL         string
	CreatedAt   string
	UpdatedAt   string

	State    WorkflowState
	Assignee *User
	Parent   *ParentRef
	Team     IssueTeamRef

	Labels           []Label
	InverseRelations []IssueRelation
	Comments         []Comment
}

// IssueTeamRef is the team reference embedded in an Issue. Issue
// queries pull only Team.ID; the adapter resolves labels and workflow
// states via the team's own queries.
type IssueTeamRef struct {
	ID string
}

// Comment mirrors Linear's GraphQL Comment type.
type Comment struct {
	ID        string
	Body      string
	CreatedAt string
	User      *User
}

// IssueConnection is the paginated result of an issues(...) query.
type IssueConnection struct {
	Nodes    []Issue
	PageInfo PageInfo
}

// CommentConnection is the paginated result of an issue.comments(...)
// query.
type CommentConnection struct {
	Nodes    []Comment
	PageInfo PageInfo
}

// IssueLabelsResult is the compound return of QueryIssueLabels: the
// issue's current label set plus the team UUID needed for downstream
// label-create or team-labels-cache refresh.
type IssueLabelsResult struct {
	TeamID string
	Labels []Label
}
