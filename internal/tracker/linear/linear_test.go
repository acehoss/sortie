package linear_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/tracker/linear"
	"github.com/sortie-ai/sortie/internal/tracker/linear/linearmock"
)

func newAdapter(t *testing.T) (*linear.LinearAdapter, *linearmock.Fake) {
	t.Helper()
	fake := linearmock.NewWithSampleData()
	adapter := linear.NewAdapterForTest(fake, "ENG", []string{"Backlog", "Todo"}, "")
	return adapter, fake
}

func TestFetchCandidateIssues_ReturnsActiveStatesOnly(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len(issues) = %d, want 2 (ENG-1 Backlog, ENG-2 Todo)", len(issues))
	}
	if issues[0].Identifier != "ENG-1" || issues[1].Identifier != "ENG-2" {
		t.Errorf("identifiers = [%s %s], want [ENG-1 ENG-2]", issues[0].Identifier, issues[1].Identifier)
	}
	for _, iss := range issues {
		if iss.Comments != nil {
			t.Errorf("candidate %s has non-nil Comments; want nil to defer comment fetch", iss.Identifier)
		}
	}
}

func TestResolveAssignee_MeQueriesViewer(t *testing.T) {
	t.Parallel()
	fake := linearmock.New()
	fake.ViewerID = "user-resolved-from-viewer"

	got, err := linear.ResolveAssigneeForTest(fake, "me")
	if err != nil {
		t.Fatalf("ResolveAssignee(me): %v", err)
	}
	if got != "user-resolved-from-viewer" {
		t.Errorf("got = %q, want user-resolved-from-viewer", got)
	}
}

func TestResolveAssignee_UUIDPassthroughSkipsViewer(t *testing.T) {
	t.Parallel()
	fake := linearmock.New()
	// ViewerID intentionally left empty — a viewer call would error.
	// A non-"me" string must be returned unchanged without that call.
	got, err := linear.ResolveAssigneeForTest(fake, "deadbeef-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("ResolveAssignee(uuid): %v", err)
	}
	if got != "deadbeef-0000-0000-0000-000000000000" {
		t.Errorf("got = %q, want UUID returned unchanged", got)
	}
}

func TestResolveAssignee_EmptyConfigReturnsEmpty(t *testing.T) {
	t.Parallel()
	fake := linearmock.New()
	got, err := linear.ResolveAssigneeForTest(fake, nil)
	if err != nil {
		t.Fatalf("ResolveAssignee(nil): %v", err)
	}
	if got != "" {
		t.Errorf("got = %q, want empty (no filter)", got)
	}
}

func TestFetchCandidateIssues_AssigneeFilterDropsUnassigned(t *testing.T) {
	t.Parallel()
	fake := linearmock.NewWithSampleData()
	// Assign ENG-2 to the configured assignee; leave ENG-1 unassigned.
	const me = "user-me"
	fake.WithLock(func() {
		fake.Issues["iss-2"].Assignee = &linear.User{ID: me, DisplayName: "Me"}
	})

	adapter := linear.NewAdapterForTest(fake, "ENG", []string{"Backlog", "Todo"}, me)
	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "ENG-2" {
		ids := make([]string, len(issues))
		for i, iss := range issues {
			ids[i] = iss.Identifier
		}
		t.Errorf("candidates = %v, want [ENG-2] (only the assigned issue)", ids)
	}
}

func TestFetchCandidateIssues_NormalizesLabelsLowercase(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	var eng2 *domain.Issue
	for i := range issues {
		if issues[i].Identifier == "ENG-2" {
			eng2 = &issues[i]
		}
	}
	if eng2 == nil {
		t.Fatalf("ENG-2 not in candidate list")
	}
	if len(eng2.Labels) != 1 || eng2.Labels[0] != "bug" {
		t.Errorf("ENG-2 labels = %v, want [bug] (lowercased)", eng2.Labels)
	}
}

func TestFetchCandidateIssues_PaginatesAcrossPages(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)

	// Seed enough extra Backlog issues to force at least two pages.
	for i := 4; i < 60; i++ {
		fake.AddIssue(makeIssue(i, "team-eng", "Backlog"))
	}

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	// 2 seed issues in active states + 56 new Backlog = 58.
	if len(issues) != 58 {
		t.Errorf("len(issues) = %d, want 58", len(issues))
	}
}

func TestFetchIssueByID_ReturnsFullPopulationIncludingComments(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)
	fake.AddComment("iss-1", makeComment("c-seed-1", "first", "alice"))
	fake.AddComment("iss-1", makeComment("c-seed-2", "second", "bob"))

	issue, err := adapter.FetchIssueByID(context.Background(), "ENG-1")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	if issue.Identifier != "ENG-1" {
		t.Errorf("identifier = %q, want ENG-1", issue.Identifier)
	}
	if len(issue.Comments) != 2 {
		t.Fatalf("len(Comments) = %d, want 2", len(issue.Comments))
	}
	if issue.Comments[0].Author != "alice" || issue.Comments[1].Body != "second" {
		t.Errorf("comments = %+v", issue.Comments)
	}
}

func TestFetchIssueByID_NotFoundSurfacesAsTrackerError(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)

	_, err := adapter.FetchIssueByID(context.Background(), "ENG-9999")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("err = %v, want ErrTrackerNotFound", err)
	}
}

func TestFetchIssuesByStates_EmptyInputReturnsEmptySlice(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)

	issues, err := adapter.FetchIssuesByStates(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if issues == nil {
		t.Errorf("returned nil slice, want empty non-nil")
	}
	if len(issues) != 0 {
		t.Errorf("len(issues) = %d, want 0", len(issues))
	}
}

func TestFetchIssuesByStates_TerminalCleanup(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)
	fake.AddIssue(makeIssue(99, "team-eng", "Done"))

	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"Done", "Canceled"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "ENG-99" {
		t.Errorf("issues = %v, want [ENG-99]", issues)
	}
}

func TestFetchIssueStatesByIDs_OmitsMissing(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)

	states, err := adapter.FetchIssueStatesByIDs(context.Background(), []string{"iss-1", "iss-3", "iss-missing"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("len(states) = %d, want 2", len(states))
	}
	if states["iss-1"] != "Backlog" || states["iss-3"] != "In Progress" {
		t.Errorf("states = %v", states)
	}
}

func TestFetchIssueStatesByIdentifiers_OmitsMissing(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)

	states, err := adapter.FetchIssueStatesByIdentifiers(context.Background(), []string{"ENG-1", "ENG-3", "ENG-999"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("len(states) = %d, want 2", len(states))
	}
	if states["ENG-1"] != "Backlog" || states["ENG-3"] != "In Progress" {
		t.Errorf("states = %v", states)
	}
}

func TestTransitionIssue_ResolvesStateAndApplies(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)

	if err := adapter.TransitionIssue(context.Background(), "iss-1", "In Progress"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}
	if state := fake.Issues["iss-1"].State.Name; state != "In Progress" {
		t.Errorf("post-transition state = %q, want In Progress", state)
	}
}

func TestTransitionIssue_UnknownStateReturnsPayloadError(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)

	err := adapter.TransitionIssue(context.Background(), "iss-1", "Frobnicating")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerPayload {
		t.Errorf("err = %v, want ErrTrackerPayload", err)
	}
}

func TestCommentIssue_PostsAndCanBeFetched(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)
	ctx := context.Background()

	if err := adapter.CommentIssue(ctx, "ENG-1", "from adapter"); err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}
	comments, err := adapter.FetchIssueComments(ctx, "ENG-1")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(comments) != 1 || comments[0].Body != "from adapter" {
		t.Errorf("comments = %+v", comments)
	}
}

func TestAddLabel_ExistingTeamLabel_AttachesWithoutCreate(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)

	createCalls := 0
	fake.MutationIssueLabelCreateFunc = func(ctx context.Context, teamID, name string) (string, error) {
		createCalls++
		return "", fmt.Errorf("unexpected create call: %s/%s", teamID, name)
	}

	if err := adapter.AddLabel(context.Background(), "iss-1", "bug"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if createCalls != 0 {
		t.Errorf("MutationIssueLabelCreate called %d times; want 0 (label already in team)", createCalls)
	}

	labels := fake.Issues["iss-1"].Labels
	if len(labels) != 1 || labels[0].Name != "bug" {
		t.Errorf("post-attach labels = %v, want [bug]", labels)
	}
}

func TestAddLabel_CreatesMissingLabel(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)

	if err := adapter.AddLabel(context.Background(), "iss-1", "needs-human"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}

	found := false
	for _, lbl := range fake.Teams["team-eng"].Labels {
		if lbl.Name == "needs-human" {
			found = true
		}
	}
	if !found {
		t.Errorf("team labels = %v; needs-human not created", fake.Teams["team-eng"].Labels)
	}

	labels := fake.Issues["iss-1"].Labels
	if len(labels) != 1 || labels[0].Name != "needs-human" {
		t.Errorf("post-attach iss-1 labels = %v, want [needs-human]", labels)
	}
}

func TestAddLabel_PreservesExistingLabels(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)

	// ENG-2 already has "bug"; add "needs-human" and ensure bug stays.
	if err := adapter.AddLabel(context.Background(), "ENG-2", "needs-human"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	labels := fake.Issues["iss-2"].Labels
	if len(labels) != 2 {
		t.Errorf("len(labels) = %d, want 2 (bug + needs-human)", len(labels))
	}
	names := map[string]bool{}
	for _, lbl := range labels {
		names[lbl.Name] = true
	}
	if !names["bug"] || !names["needs-human"] {
		t.Errorf("labels = %v; want both bug and needs-human", labels)
	}
}

func TestAddLabel_AlreadyAttachedIsNoOp(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)

	updateCalls := 0
	fake.MutationIssueUpdateLabelsFunc = func(ctx context.Context, issueID string, labelIDs []string) error {
		updateCalls++
		return fmt.Errorf("unexpected update call: %s %v", issueID, labelIDs)
	}

	if err := adapter.AddLabel(context.Background(), "ENG-2", "bug"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if updateCalls != 0 {
		t.Errorf("MutationIssueUpdateLabels called %d times; want 0 (idempotent)", updateCalls)
	}
}

func TestAddLabel_HandlesAlreadyExistsRace(t *testing.T) {
	t.Parallel()
	adapter, fake := newAdapter(t)

	// Simulate the race: create reports "already exists" because a
	// concurrent writer inserted the label between our cache refresh
	// and our create. The fake's team-labels store reflects the
	// concurrent insertion before the create returns.
	calls := 0
	fake.MutationIssueLabelCreateFunc = func(ctx context.Context, teamID, name string) (string, error) {
		calls++
		fake.WithLock(func() {
			fake.Teams[teamID].Labels = append(fake.Teams[teamID].Labels,
				labelOf("lbl-race", name))
		})
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("label %q already exists in team %q", name, "ENG"),
		}
	}

	if err := adapter.AddLabel(context.Background(), "iss-1", "needs-human"); err != nil {
		t.Fatalf("AddLabel race recovery: %v", err)
	}
	if calls != 1 {
		t.Errorf("create attempts = %d, want 1", calls)
	}
	labels := fake.Issues["iss-1"].Labels
	if len(labels) != 1 || labels[0].ID != "lbl-race" || labels[0].Name != "needs-human" {
		t.Errorf("post-attach labels = %+v, want [{lbl-race needs-human}]", labels)
	}
}

func TestSetMetrics_RecordsOperationOutcomes(t *testing.T) {
	t.Parallel()
	adapter, _ := newAdapter(t)
	spy := &spyMetrics{}
	adapter.SetMetrics(spy)

	if _, err := adapter.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if _, err := adapter.FetchIssueByID(context.Background(), "ENG-9999"); err == nil {
		t.Fatalf("expected error for missing issue")
	}

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.calls) != 2 {
		t.Fatalf("metric calls = %d, want 2; got %+v", len(spy.calls), spy.calls)
	}
	if spy.calls[0].operation != "fetch_candidates" || spy.calls[0].result != "success" {
		t.Errorf("call[0] = %+v", spy.calls[0])
	}
	if spy.calls[1].operation != "fetch_issue" || spy.calls[1].result != "error" {
		t.Errorf("call[1] = %+v", spy.calls[1])
	}
}

// ───── test helpers ───────────────────────────────────────────────────

type spyMetrics struct {
	domain.NoopMetrics
	mu    sync.Mutex
	calls []trackerRequestCall
}

type trackerRequestCall struct{ operation, result string }

func (s *spyMetrics) IncTrackerRequests(operation, result string) {
	s.mu.Lock()
	s.calls = append(s.calls, trackerRequestCall{operation, result})
	s.mu.Unlock()
}

func makeIssue(n int, teamID, stateName string) *linear.Issue {
	ident := fmt.Sprintf("ENG-%d", n)
	return &linear.Issue{
		ID:         fmt.Sprintf("iss-%d", n),
		Identifier: ident,
		Title:      fmt.Sprintf("Issue %d", n),
		Priority:   2,
		BranchName: "eng/" + strings.ToLower(ident),
		URL:        "https://linear.app/example/issue/" + ident,
		CreatedAt:  "2026-01-04T00:00:00Z",
		UpdatedAt:  "2026-01-04T00:00:00Z",
		State:      workflowStateFor(stateName),
		Team:       linear.IssueTeamRef{ID: teamID},
	}
}

func workflowStateFor(name string) linear.WorkflowState {
	switch name {
	case "Backlog":
		return linear.WorkflowState{ID: "ws-backlog", Name: "Backlog", Type: "backlog"}
	case "Todo":
		return linear.WorkflowState{ID: "ws-todo", Name: "Todo", Type: "unstarted"}
	case "In Progress":
		return linear.WorkflowState{ID: "ws-in-progress", Name: "In Progress", Type: "started"}
	case "Done":
		return linear.WorkflowState{ID: "ws-done", Name: "Done", Type: "completed"}
	case "Canceled":
		return linear.WorkflowState{ID: "ws-canceled", Name: "Canceled", Type: "canceled"}
	}
	return linear.WorkflowState{Name: name}
}

func makeComment(id, body, author string) linear.Comment {
	return linear.Comment{
		ID:        id,
		Body:      body,
		CreatedAt: "2026-01-01T00:00:00Z",
		User:      &linear.User{DisplayName: author},
	}
}

func labelOf(id, name string) linear.Label {
	return linear.Label{ID: id, Name: name}
}
