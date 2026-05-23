package linearmock

import (
	"fmt"

	"github.com/sortie-ai/sortie/internal/tracker/linear"
)

// NewWithSampleData returns a Fake pre-populated with a single team
// ("ENG"), its standard workflow states (Backlog, Todo, In Progress,
// In Review, Done, Canceled), a `bug` label, and three issues spanning
// Backlog, Todo, and In Progress. Useful for adapter tests that need a
// realistic baseline without per-test seeding.
//
// Tests that need different shapes should call [New] and seed via
// helper functions or direct field access under [Fake.WithLock].
func NewWithSampleData() *Fake {
	f := New()
	teamID := f.AddTeam("team-eng", "ENG",
		[]linear.WorkflowState{
			{ID: "ws-backlog", Name: "Backlog", Type: "backlog"},
			{ID: "ws-todo", Name: "Todo", Type: "unstarted"},
			{ID: "ws-in-progress", Name: "In Progress", Type: "started"},
			{ID: "ws-in-review", Name: "In Review", Type: "started"},
			{ID: "ws-done", Name: "Done", Type: "completed"},
			{ID: "ws-canceled", Name: "Canceled", Type: "canceled"},
		},
		[]linear.Label{
			{ID: "lbl-bug", Name: "bug"},
		},
	)
	f.AddIssue(&linear.Issue{
		ID:         "iss-1",
		Identifier: "ENG-1",
		Title:      "First backlog item",
		Priority:   2,
		BranchName: "eng/eng-1-first-backlog-item",
		URL:        "https://linear.app/example/issue/ENG-1",
		CreatedAt:  "2026-01-01T00:00:00Z",
		UpdatedAt:  "2026-01-01T00:00:00Z",
		State:      linear.WorkflowState{ID: "ws-backlog", Name: "Backlog", Type: "backlog"},
		Team:       linear.IssueTeamRef{ID: teamID},
	})
	f.AddIssue(&linear.Issue{
		ID:         "iss-2",
		Identifier: "ENG-2",
		Title:      "Ready to pick up",
		Priority:   1,
		BranchName: "eng/eng-2-ready-to-pick-up",
		URL:        "https://linear.app/example/issue/ENG-2",
		CreatedAt:  "2026-01-02T00:00:00Z",
		UpdatedAt:  "2026-01-02T00:00:00Z",
		State:      linear.WorkflowState{ID: "ws-todo", Name: "Todo", Type: "unstarted"},
		Team:       linear.IssueTeamRef{ID: teamID},
		Labels:     []linear.Label{{ID: "lbl-bug", Name: "bug"}},
	})
	f.AddIssue(&linear.Issue{
		ID:         "iss-3",
		Identifier: "ENG-3",
		Title:      "Already running",
		Priority:   3,
		BranchName: "eng/eng-3-already-running",
		URL:        "https://linear.app/example/issue/ENG-3",
		CreatedAt:  "2026-01-03T00:00:00Z",
		UpdatedAt:  "2026-01-03T00:00:00Z",
		State:      linear.WorkflowState{ID: "ws-in-progress", Name: "In Progress", Type: "started"},
		Team:       linear.IssueTeamRef{ID: teamID},
	})
	return f
}

// AddTeam registers a team with the given UUID, key, workflow states,
// and labels, then returns the team UUID. Convenience for tests that
// want to seed multiple teams or customize the standard set.
func (f *Fake) AddTeam(id, key string, states []linear.WorkflowState, labels []linear.Label) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Teams[id] = &TeamData{
		Key:            key,
		WorkflowStates: append([]linear.WorkflowState(nil), states...),
		Labels:         append([]linear.Label(nil), labels...),
	}
	return id
}

// AddIssue registers an issue. The issue is indexed by both its UUID
// and its human identifier for lookups. Panics if Issue.ID is empty
// (tests should always supply a deterministic ID).
func (f *Fake) AddIssue(iss *linear.Issue) {
	if iss.ID == "" {
		panic(fmt.Sprintf("linearmock: AddIssue requires non-empty Issue.ID; identifier=%q", iss.Identifier))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *iss
	clone.Labels = append([]linear.Label(nil), iss.Labels...)
	clone.InverseRelations = append([]linear.IssueRelation(nil), iss.InverseRelations...)
	f.Issues[iss.ID] = &clone
}

// AddComment appends a comment to the given issue's comment list.
// Used by tests that want to seed pre-existing comments without
// going through MutationCommentCreate.
func (f *Fake) AddComment(issueID string, comment linear.Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Comments[issueID] = append(f.Comments[issueID], comment)
}
