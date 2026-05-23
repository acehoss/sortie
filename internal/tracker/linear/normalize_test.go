package linear

import (
	"testing"
)

func TestNormalizePriority_NoPriorityBecomesNil(t *testing.T) {
	t.Parallel()
	if got := normalizePriority(0); got != nil {
		t.Errorf("normalizePriority(0) = %v, want nil (Linear's No-priority)", got)
	}
}

func TestNormalizePriority_OutOfRangeBecomesNil(t *testing.T) {
	t.Parallel()
	cases := []float64{-1, 5, 100, 4.5}
	for _, p := range cases {
		if got := normalizePriority(p); got != nil {
			t.Errorf("normalizePriority(%v) = %v, want nil", p, got)
		}
	}
}

func TestNormalizePriority_ValidValuesPassThrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want int
	}{{1, 1}, {2, 2}, {3, 3}, {4, 4}}
	for _, tc := range cases {
		got := normalizePriority(tc.in)
		if got == nil || *got != tc.want {
			t.Errorf("normalizePriority(%v) = %v, want %d", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeAssignee_PrefersDisplayNameOverNameOverEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   *User
		want string
	}{
		{"nil user", nil, ""},
		{"all empty", &User{}, ""},
		{"display name wins", &User{DisplayName: "Alice", Name: "alice", Email: "a@e.com"}, "Alice"},
		{"name when display empty", &User{Name: "alice", Email: "a@e.com"}, "alice"},
		{"email when name empty", &User{Email: "a@e.com"}, "a@e.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeAssignee(tc.in); got != tc.want {
				t.Errorf("normalizeAssignee = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeIssue_LowercasesLabelsAndPreservesOrder(t *testing.T) {
	t.Parallel()
	src := &Issue{
		ID:         "iss-1",
		Identifier: "ENG-1",
		Title:      "title",
		Priority:   2,
		Labels: []Label{
			{ID: "lbl-1", Name: "Bug"},
			{ID: "lbl-2", Name: "URGENT"},
		},
	}
	got := normalizeIssue(src)

	if len(got.Labels) != 2 {
		t.Fatalf("labels len = %d, want 2", len(got.Labels))
	}
	if got.Labels[0] != "bug" || got.Labels[1] != "urgent" {
		t.Errorf("labels = %v, want [bug urgent]", got.Labels)
	}
}

func TestNormalizeIssue_BlockedByOnlyIncludesBlocksRelations(t *testing.T) {
	t.Parallel()
	src := &Issue{
		ID:         "iss-1",
		Identifier: "ENG-1",
		InverseRelations: []IssueRelation{
			{Type: "blocks", Issue: IssueRef{ID: "iss-2", Identifier: "ENG-2", State: "In Progress"}},
			{Type: "related", Issue: IssueRef{ID: "iss-3", Identifier: "ENG-3", State: "Done"}},
			{Type: "BLOCKS", Issue: IssueRef{ID: "iss-4", Identifier: "ENG-4", State: "Todo"}}, // case-insensitive
			{Type: "duplicate", Issue: IssueRef{ID: "iss-5", Identifier: "ENG-5", State: "Done"}},
		},
	}
	got := normalizeIssue(src)

	if len(got.BlockedBy) != 2 {
		t.Fatalf("blocked_by len = %d, want 2", len(got.BlockedBy))
	}
	if got.BlockedBy[0].Identifier != "ENG-2" || got.BlockedBy[1].Identifier != "ENG-4" {
		t.Errorf("blocked_by identifiers = [%s %s], want [ENG-2 ENG-4]",
			got.BlockedBy[0].Identifier, got.BlockedBy[1].Identifier)
	}
}

func TestNormalizeIssue_NilOptionalFieldsBecomeEmpty(t *testing.T) {
	t.Parallel()
	src := &Issue{
		ID:         "iss-1",
		Identifier: "ENG-1",
		Title:      "no extras",
		// Priority 0 (no priority), Assignee nil, Parent nil, no labels, no relations.
	}
	got := normalizeIssue(src)

	if got.Priority != nil {
		t.Errorf("Priority = %v, want nil for priority 0", got.Priority)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty", got.Assignee)
	}
	if got.Parent != nil {
		t.Errorf("Parent = %v, want nil", got.Parent)
	}
	if got.Labels == nil {
		t.Errorf("Labels = nil, want empty non-nil slice")
	}
	if got.BlockedBy == nil {
		t.Errorf("BlockedBy = nil, want empty non-nil slice")
	}
}

func TestNormalizeIssue_ParentMapsCorrectly(t *testing.T) {
	t.Parallel()
	src := &Issue{
		ID:         "iss-child",
		Identifier: "ENG-CHILD",
		Parent:     &ParentRef{ID: "iss-parent", Identifier: "ENG-PARENT"},
	}
	got := normalizeIssue(src)

	if got.Parent == nil {
		t.Fatalf("Parent = nil, want populated")
	}
	if got.Parent.ID != "iss-parent" || got.Parent.Identifier != "ENG-PARENT" {
		t.Errorf("Parent = %+v", got.Parent)
	}
}

func TestNormalizeIssue_StatePullsNameFromNestedWorkflowState(t *testing.T) {
	t.Parallel()
	src := &Issue{
		ID:    "iss-1",
		State: WorkflowState{ID: "ws-todo", Name: "Todo", Type: "unstarted"},
	}
	got := normalizeIssue(src)

	if got.State != "Todo" {
		t.Errorf("State = %q, want Todo", got.State)
	}
}

func TestNormalizeComment_PullsAuthorFromNestedUser(t *testing.T) {
	t.Parallel()
	got := normalizeComment(Comment{
		ID:        "c-1",
		Body:      "review note",
		CreatedAt: "2026-01-01T00:00:00Z",
		User:      &User{DisplayName: "Alice"},
	})

	if got.Author != "Alice" || got.Body != "review note" || got.ID != "c-1" {
		t.Errorf("normalizeComment = %+v", got)
	}
}

func TestNormalizeComment_NilUserMeansEmptyAuthor(t *testing.T) {
	t.Parallel()
	got := normalizeComment(Comment{ID: "c-1", Body: "from a bot", User: nil})
	if got.Author != "" {
		t.Errorf("Author = %q, want empty for nil user (bot/integration)", got.Author)
	}
}
