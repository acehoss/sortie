package linearmock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/tracker/linear"
	"github.com/sortie-ai/sortie/internal/tracker/linear/linearmock"
)

func TestNewWithSampleData_SeedsExpectedShape(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	if got := len(f.Issues); got != 3 {
		t.Errorf("Issues count = %d, want 3", got)
	}
	if got := len(f.Teams); got != 1 {
		t.Errorf("Teams count = %d, want 1", got)
	}
	team := f.Teams["team-eng"]
	if team == nil {
		t.Fatalf("expected team-eng to be registered")
	}
	if team.Key != "ENG" {
		t.Errorf("team key = %q, want %q", team.Key, "ENG")
	}
	if len(team.WorkflowStates) != 6 {
		t.Errorf("workflow state count = %d, want 6", len(team.WorkflowStates))
	}
}

func TestQueryIssues_FiltersByTeamAndState(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()
	ctx := context.Background()

	conn, err := f.QueryIssues(ctx, linear.IssuesFilter{
		TeamKey:    "ENG",
		StateNames: []string{"Backlog", "Todo"},
	}, 50, "")
	if err != nil {
		t.Fatalf("QueryIssues: %v", err)
	}
	if len(conn.Nodes) != 2 {
		t.Fatalf("nodes count = %d, want 2", len(conn.Nodes))
	}
	if conn.Nodes[0].Identifier != "ENG-1" || conn.Nodes[1].Identifier != "ENG-2" {
		t.Errorf("nodes ordering = [%s, %s], want [ENG-1, ENG-2]",
			conn.Nodes[0].Identifier, conn.Nodes[1].Identifier)
	}
	if conn.PageInfo.HasNextPage {
		t.Errorf("HasNextPage = true, want false")
	}
}

func TestQueryIssues_StateMatchIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	conn, err := f.QueryIssues(context.Background(), linear.IssuesFilter{
		TeamKey:    "ENG",
		StateNames: []string{"backlog", "TODO"},
	}, 50, "")
	if err != nil {
		t.Fatalf("QueryIssues: %v", err)
	}
	if len(conn.Nodes) != 2 {
		t.Errorf("nodes count = %d, want 2 (case-insensitive state match)", len(conn.Nodes))
	}
}

func TestQueryIssues_WrongTeamReturnsEmpty(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	conn, err := f.QueryIssues(context.Background(), linear.IssuesFilter{
		TeamKey: "OTHER",
	}, 50, "")
	if err != nil {
		t.Fatalf("QueryIssues: %v", err)
	}
	if len(conn.Nodes) != 0 {
		t.Errorf("nodes count = %d, want 0", len(conn.Nodes))
	}
}

func TestQueryIssues_PaginatesWithCursor(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	page1, err := f.QueryIssues(context.Background(), linear.IssuesFilter{TeamKey: "ENG"}, 2, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Nodes) != 2 || !page1.PageInfo.HasNextPage || page1.PageInfo.EndCursor == "" {
		t.Fatalf("page1 = %+v; want 2 nodes with cursor", page1)
	}
	page2, err := f.QueryIssues(context.Background(), linear.IssuesFilter{TeamKey: "ENG"}, 2, page1.PageInfo.EndCursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Nodes) != 1 || page2.PageInfo.HasNextPage {
		t.Errorf("page2 = %+v; want 1 node, no next page", page2)
	}
}

func TestQueryIssueByKey_AcceptsUUIDAndIdentifier(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	byUUID, err := f.QueryIssueByKey(context.Background(), "iss-2")
	if err != nil {
		t.Fatalf("by UUID: %v", err)
	}
	byIdent, err := f.QueryIssueByKey(context.Background(), "ENG-2")
	if err != nil {
		t.Fatalf("by identifier: %v", err)
	}
	if byUUID.ID != byIdent.ID || byUUID.Identifier != byIdent.Identifier {
		t.Errorf("lookup by UUID vs identifier returned different issues: %v vs %v", byUUID, byIdent)
	}
}

func TestQueryIssueByKey_NotFoundReturnsTrackerError(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	_, err := f.QueryIssueByKey(context.Background(), "ENG-999")
	var trackerErr *domain.TrackerError
	if !errors.As(err, &trackerErr) || trackerErr.Kind != domain.ErrTrackerNotFound {
		t.Errorf("err = %v (kind = %v), want ErrTrackerNotFound", err, trackerErr)
	}
}

func TestQueryIssueStatesByKeys_OmitsMissing(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	states, err := f.QueryIssueStatesByKeys(context.Background(),
		[]string{"iss-1", "ENG-2", "ENG-999"})
	if err != nil {
		t.Fatalf("QueryIssueStatesByKeys: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("states map size = %d, want 2 (missing keys omitted)", len(states))
	}
	if states["iss-1"] != "Backlog" {
		t.Errorf("iss-1 state = %q, want Backlog", states["iss-1"])
	}
	if states["ENG-2"] != "Todo" {
		t.Errorf("ENG-2 state = %q, want Todo", states["ENG-2"])
	}
}

func TestQueryStateIDByName_CaseInsensitive(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	id, err := f.QueryStateIDByName(context.Background(), "ENG-1", "in review")
	if err != nil {
		t.Fatalf("QueryStateIDByName: %v", err)
	}
	if id != "ws-in-review" {
		t.Errorf("state ID = %q, want ws-in-review", id)
	}
}

func TestQueryStateIDByName_UnknownStateReturnsPayloadError(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	_, err := f.QueryStateIDByName(context.Background(), "ENG-1", "Frobnicating")
	var trackerErr *domain.TrackerError
	if !errors.As(err, &trackerErr) || trackerErr.Kind != domain.ErrTrackerPayload {
		t.Errorf("err = %v (kind = %v), want ErrTrackerPayload", err, trackerErr)
	}
}

func TestMutationIssueUpdateState_AppliesAndPersists(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()
	ctx := context.Background()

	if err := f.MutationIssueUpdateState(ctx, "iss-1", "ws-in-progress"); err != nil {
		t.Fatalf("MutationIssueUpdateState: %v", err)
	}
	iss, err := f.QueryIssueByKey(ctx, "iss-1")
	if err != nil {
		t.Fatalf("QueryIssueByKey: %v", err)
	}
	if iss.State.ID != "ws-in-progress" || iss.State.Name != "In Progress" {
		t.Errorf("post-update state = %+v, want In Progress", iss.State)
	}
}

func TestMutationCommentCreate_AppendsAndReturnsID(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()
	ctx := context.Background()

	id, err := f.MutationCommentCreate(ctx, "ENG-2", "first review note")
	if err != nil {
		t.Fatalf("MutationCommentCreate: %v", err)
	}
	if id == "" {
		t.Errorf("comment id is empty")
	}

	iss, err := f.QueryIssueByKey(ctx, "ENG-2")
	if err != nil {
		t.Fatalf("QueryIssueByKey: %v", err)
	}
	if len(iss.Comments) != 1 || iss.Comments[0].Body != "first review note" {
		t.Errorf("comments after create = %v", iss.Comments)
	}
}

func TestMutationIssueLabelCreate_AddsToTeamThenAttachableViaUpdateLabels(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()
	ctx := context.Background()

	labelID, err := f.MutationIssueLabelCreate(ctx, "team-eng", "needs-human")
	if err != nil {
		t.Fatalf("MutationIssueLabelCreate: %v", err)
	}
	if labelID == "" {
		t.Errorf("returned label id is empty")
	}

	teamLabels, err := f.QueryTeamLabels(ctx, "team-eng")
	if err != nil {
		t.Fatalf("QueryTeamLabels: %v", err)
	}
	if len(teamLabels) != 2 {
		t.Errorf("team labels count = %d, want 2 (bug + needs-human)", len(teamLabels))
	}

	if err := f.MutationIssueUpdateLabels(ctx, "iss-1", []string{labelID}); err != nil {
		t.Fatalf("MutationIssueUpdateLabels: %v", err)
	}
	res, err := f.QueryIssueLabels(ctx, "iss-1")
	if err != nil {
		t.Fatalf("QueryIssueLabels: %v", err)
	}
	if len(res.Labels) != 1 || res.Labels[0].Name != "needs-human" {
		t.Errorf("issue labels = %v, want [needs-human]", res.Labels)
	}
}

func TestMutationIssueLabelCreate_DuplicateNameReturnsPayloadError(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	_, err := f.MutationIssueLabelCreate(context.Background(), "team-eng", "Bug") // case-insensitive collide with seed "bug"
	var trackerErr *domain.TrackerError
	if !errors.As(err, &trackerErr) || trackerErr.Kind != domain.ErrTrackerPayload {
		t.Errorf("err = %v, want ErrTrackerPayload for duplicate", err)
	}
}

func TestQueryIssueLabels_ReturnsTeamIDAndCurrentLabels(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	res, err := f.QueryIssueLabels(context.Background(), "ENG-2")
	if err != nil {
		t.Fatalf("QueryIssueLabels: %v", err)
	}
	if res.TeamID != "team-eng" {
		t.Errorf("team id = %q, want team-eng", res.TeamID)
	}
	if len(res.Labels) != 1 || res.Labels[0].Name != "bug" {
		t.Errorf("labels = %v, want [bug]", res.Labels)
	}
}

func TestOverrideHook_InterceptsCall(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()

	calls := 0
	sentinel := errors.New("injected")
	f.QueryIssueByKeyFunc = func(ctx context.Context, key string) (*linear.Issue, error) {
		calls++
		return nil, sentinel
	}

	_, err := f.QueryIssueByKey(context.Background(), "ENG-1")
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel via override", err)
	}
	if calls != 1 {
		t.Errorf("override invocations = %d, want 1", calls)
	}
}

func TestContextCancellation_PropagatesToReadMethods(t *testing.T) {
	t.Parallel()
	f := linearmock.NewWithSampleData()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.QueryIssues(ctx, linear.IssuesFilter{TeamKey: "ENG"}, 50, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("QueryIssues err = %v, want context.Canceled", err)
	}
	if _, err := f.QueryIssueByKey(ctx, "ENG-1"); !errors.Is(err, context.Canceled) {
		t.Errorf("QueryIssueByKey err = %v, want context.Canceled", err)
	}
}
