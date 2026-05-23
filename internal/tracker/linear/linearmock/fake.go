// Package linearmock provides a stateful in-memory fake of
// [linear.Client] for tests.
//
// The fake serves two roles:
//
//   - As a drop-in replacement for the real client in adapter tests:
//     pre-populate it with issues, labels, and workflow states; let the
//     adapter under test exercise queries and mutations against that
//     state; assert against the resulting state.
//
//   - As the place where Linear's quirks are encoded centrally. When
//     UAT against the real API reveals a divergence, fix it in the
//     fake first to align it with reality, then fix the adapter and
//     its tests against the updated fake.
//
// Each interface method has a corresponding `FooFunc` field. When set,
// the fake calls the override instead of running its default stateful
// behavior. Use the override hooks for tests that want to inject error
// returns, unusual payloads, or behavior the default state machine
// does not model.
package linearmock

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/tracker/linear"
)

// Fake is a thread-safe in-memory implementation of [linear.Client].
//
// Zero value is unusable; construct via [New] or [NewWithSampleData].
//
// All state mutations and reads are serialized through mu, which
// matches the concurrency contract on the real client. Tests may read
// or modify the exported fields directly while holding the mutex via
// [Fake.WithLock] for setup, but must use the interface methods (which
// take the lock internally) when simulating client traffic.
type Fake struct {
	mu sync.Mutex

	// Issues is the issue store keyed by UUID. The fake also indexes by
	// human identifier internally; both forms work as keys to QueryIssueByKey
	// and QueryIssueStatesByKeys.
	Issues map[string]*linear.Issue

	// Teams is the per-team registry of workflow states and labels.
	// Keyed by team UUID.
	Teams map[string]*TeamData

	// Comments holds per-issue comment lists keyed by issue UUID.
	// QueryIssueByKey copies the first page into Issue.Comments;
	// callers also add comments directly via MutationCommentCreate.
	Comments map[string][]linear.Comment

	// Counters used to mint deterministic IDs for newly-created
	// labels and comments inside mutations. Tests asserting against
	// these can pre-seed the counters.
	NextLabelID   int
	NextCommentID int

	// Override hooks. When non-nil, the fake calls the hook instead
	// of running its default stateful behavior. Each hook has the
	// same signature as the corresponding Client method.
	QueryIssuesFunc                func(ctx context.Context, filter linear.IssuesFilter, first int, after string) (*linear.IssueConnection, error)
	QueryIssueByKeyFunc            func(ctx context.Context, key string) (*linear.Issue, error)
	QueryIssueStatesByKeysFunc     func(ctx context.Context, keys []string) (map[string]string, error)
	QueryIssueCommentsFunc         func(ctx context.Context, issueID string, first int, after string) (*linear.CommentConnection, error)
	QueryStateIDByNameFunc         func(ctx context.Context, issueID string, stateName string) (string, error)
	QueryIssueLabelsFunc           func(ctx context.Context, issueID string) (*linear.IssueLabelsResult, error)
	QueryTeamLabelsFunc            func(ctx context.Context, teamID string) ([]linear.Label, error)
	MutationIssueUpdateStateFunc   func(ctx context.Context, issueID string, stateID string) error
	MutationIssueUpdateLabelsFunc  func(ctx context.Context, issueID string, labelIDs []string) error
	MutationCommentCreateFunc      func(ctx context.Context, issueID string, body string) (string, error)
	MutationIssueLabelCreateFunc   func(ctx context.Context, teamID string, name string) (string, error)
}

// TeamData is the per-team registry of workflow states and labels.
type TeamData struct {
	Key            string
	WorkflowStates []linear.WorkflowState
	Labels         []linear.Label
}

// New constructs an empty Fake with initialized maps. Tests that want
// a populated fake should use [NewWithSampleData] or seed the fields
// directly via [Fake.WithLock].
func New() *Fake {
	return &Fake{
		Issues:        map[string]*linear.Issue{},
		Teams:         map[string]*TeamData{},
		Comments:      map[string][]linear.Comment{},
		NextLabelID:   1,
		NextCommentID: 1,
	}
}

// WithLock runs fn while holding the fake's mutex. Used by tests to
// seed or inspect the fake's internal state without racing the
// interface methods.
func (f *Fake) WithLock(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// Compile-time check that *Fake implements linear.Client.
var _ linear.Client = (*Fake)(nil)

// ───── interface methods ──────────────────────────────────────────────

func (f *Fake) QueryIssues(ctx context.Context, filter linear.IssuesFilter, first int, after string) (*linear.IssueConnection, error) {
	if f.QueryIssuesFunc != nil {
		return f.QueryIssuesFunc(ctx, filter, first, after)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	matching := make([]linear.Issue, 0, len(f.Issues))
	for _, iss := range f.Issues {
		if !matchesFilter(iss, filter, f.Teams) {
			continue
		}
		matching = append(matching, deepCopyIssue(iss, false))
	}
	sort.Slice(matching, func(i, j int) bool {
		if matching[i].CreatedAt != matching[j].CreatedAt {
			return matching[i].CreatedAt < matching[j].CreatedAt
		}
		return matching[i].ID < matching[j].ID
	})

	startIdx := 0
	if after != "" {
		for i, iss := range matching {
			if iss.ID == after {
				startIdx = i + 1
				break
			}
		}
	}
	if first <= 0 {
		first = 50
	}
	endIdx := startIdx + first
	if endIdx > len(matching) {
		endIdx = len(matching)
	}
	page := matching[startIdx:endIdx]

	conn := &linear.IssueConnection{
		Nodes: page,
		PageInfo: linear.PageInfo{
			HasNextPage: endIdx < len(matching),
		},
	}
	if conn.PageInfo.HasNextPage && len(page) > 0 {
		conn.PageInfo.EndCursor = page[len(page)-1].ID
	}
	return conn, nil
}

func (f *Fake) QueryIssueByKey(ctx context.Context, key string) (*linear.Issue, error) {
	if f.QueryIssueByKeyFunc != nil {
		return f.QueryIssueByKeyFunc(ctx, key)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	iss := f.lookupIssueLocked(key)
	if iss == nil {
		return nil, notFoundError("issue %q", key)
	}
	out := deepCopyIssue(iss, true)
	out.Comments = copyComments(f.Comments[iss.ID])
	return &out, nil
}

func (f *Fake) QueryIssueStatesByKeys(ctx context.Context, keys []string) (map[string]string, error) {
	if f.QueryIssueStatesByKeysFunc != nil {
		return f.QueryIssueStatesByKeysFunc(ctx, keys)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if iss := f.lookupIssueLocked(key); iss != nil {
			out[key] = iss.State.Name
		}
	}
	return out, nil
}

func (f *Fake) QueryIssueComments(ctx context.Context, issueID string, first int, after string) (*linear.CommentConnection, error) {
	if f.QueryIssueCommentsFunc != nil {
		return f.QueryIssueCommentsFunc(ctx, issueID, first, after)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	iss := f.lookupIssueLocked(issueID)
	if iss == nil {
		return nil, notFoundError("issue %q", issueID)
	}
	all := f.Comments[iss.ID]

	startIdx := 0
	if after != "" {
		for i, c := range all {
			if c.ID == after {
				startIdx = i + 1
				break
			}
		}
	}
	if first <= 0 {
		first = 50
	}
	endIdx := startIdx + first
	if endIdx > len(all) {
		endIdx = len(all)
	}
	page := copyComments(all[startIdx:endIdx])

	conn := &linear.CommentConnection{
		Nodes: page,
		PageInfo: linear.PageInfo{
			HasNextPage: endIdx < len(all),
		},
	}
	if conn.PageInfo.HasNextPage && len(page) > 0 {
		conn.PageInfo.EndCursor = page[len(page)-1].ID
	}
	return conn, nil
}

func (f *Fake) QueryStateIDByName(ctx context.Context, issueID string, stateName string) (string, error) {
	if f.QueryStateIDByNameFunc != nil {
		return f.QueryStateIDByNameFunc(ctx, issueID, stateName)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	iss := f.lookupIssueLocked(issueID)
	if iss == nil {
		return "", notFoundError("issue %q", issueID)
	}
	team := f.Teams[iss.Team.ID]
	if team == nil {
		return "", payloadError("issue %q has no team in fake state", issueID)
	}
	for _, state := range team.WorkflowStates {
		if strings.EqualFold(state.Name, stateName) {
			return state.ID, nil
		}
	}
	return "", payloadError("no workflow state named %q in team %q", stateName, team.Key)
}

func (f *Fake) QueryIssueLabels(ctx context.Context, issueID string) (*linear.IssueLabelsResult, error) {
	if f.QueryIssueLabelsFunc != nil {
		return f.QueryIssueLabelsFunc(ctx, issueID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	iss := f.lookupIssueLocked(issueID)
	if iss == nil {
		return nil, notFoundError("issue %q", issueID)
	}
	out := &linear.IssueLabelsResult{
		TeamID: iss.Team.ID,
		Labels: append([]linear.Label(nil), iss.Labels...),
	}
	return out, nil
}

func (f *Fake) QueryTeamLabels(ctx context.Context, teamID string) ([]linear.Label, error) {
	if f.QueryTeamLabelsFunc != nil {
		return f.QueryTeamLabelsFunc(ctx, teamID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	team := f.Teams[teamID]
	if team == nil {
		return nil, notFoundError("team %q", teamID)
	}
	return append([]linear.Label(nil), team.Labels...), nil
}

func (f *Fake) MutationIssueUpdateState(ctx context.Context, issueID string, stateID string) error {
	if f.MutationIssueUpdateStateFunc != nil {
		return f.MutationIssueUpdateStateFunc(ctx, issueID, stateID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	iss := f.lookupIssueLocked(issueID)
	if iss == nil {
		return notFoundError("issue %q", issueID)
	}
	team := f.Teams[iss.Team.ID]
	if team == nil {
		return payloadError("issue %q has no team in fake state", issueID)
	}
	for _, state := range team.WorkflowStates {
		if state.ID == stateID {
			iss.State = state
			return nil
		}
	}
	return payloadError("no workflow state %q in team %q", stateID, team.Key)
}

func (f *Fake) MutationIssueUpdateLabels(ctx context.Context, issueID string, labelIDs []string) error {
	if f.MutationIssueUpdateLabelsFunc != nil {
		return f.MutationIssueUpdateLabelsFunc(ctx, issueID, labelIDs)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	iss := f.lookupIssueLocked(issueID)
	if iss == nil {
		return notFoundError("issue %q", issueID)
	}
	team := f.Teams[iss.Team.ID]
	if team == nil {
		return payloadError("issue %q has no team in fake state", issueID)
	}
	labels := make([]linear.Label, 0, len(labelIDs))
	for _, id := range labelIDs {
		found := false
		for _, lbl := range team.Labels {
			if lbl.ID == id {
				labels = append(labels, lbl)
				found = true
				break
			}
		}
		if !found {
			return payloadError("label %q does not exist in team %q", id, team.Key)
		}
	}
	iss.Labels = labels
	return nil
}

func (f *Fake) MutationCommentCreate(ctx context.Context, issueID string, body string) (string, error) {
	if f.MutationCommentCreateFunc != nil {
		return f.MutationCommentCreateFunc(ctx, issueID, body)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	iss := f.lookupIssueLocked(issueID)
	if iss == nil {
		return "", notFoundError("issue %q", issueID)
	}
	id := fmt.Sprintf("c-%d", f.NextCommentID)
	f.NextCommentID++
	comment := linear.Comment{
		ID:        id,
		Body:      body,
		CreatedAt: fmt.Sprintf("2026-01-01T00:00:%02dZ", f.NextCommentID%60),
	}
	f.Comments[iss.ID] = append(f.Comments[iss.ID], comment)
	return id, nil
}

func (f *Fake) MutationIssueLabelCreate(ctx context.Context, teamID string, name string) (string, error) {
	if f.MutationIssueLabelCreateFunc != nil {
		return f.MutationIssueLabelCreateFunc(ctx, teamID, name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	team := f.Teams[teamID]
	if team == nil {
		return "", notFoundError("team %q", teamID)
	}
	for _, lbl := range team.Labels {
		if strings.EqualFold(lbl.Name, name) {
			return "", &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("label %q already exists in team %q", name, team.Key),
			}
		}
	}
	id := fmt.Sprintf("lbl-%d", f.NextLabelID)
	f.NextLabelID++
	team.Labels = append(team.Labels, linear.Label{ID: id, Name: name})
	return id, nil
}

// ───── internal helpers ───────────────────────────────────────────────

func (f *Fake) lookupIssueLocked(key string) *linear.Issue {
	if iss, ok := f.Issues[key]; ok {
		return iss
	}
	for _, iss := range f.Issues {
		if iss.Identifier == key {
			return iss
		}
	}
	return nil
}

func matchesFilter(iss *linear.Issue, filter linear.IssuesFilter, teams map[string]*TeamData) bool {
	if filter.TeamKey != "" {
		team, ok := teams[iss.Team.ID]
		if !ok || team.Key != filter.TeamKey {
			return false
		}
	}
	if len(filter.StateNames) > 0 {
		matched := false
		for _, name := range filter.StateNames {
			if strings.EqualFold(name, iss.State.Name) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func deepCopyIssue(src *linear.Issue, includeComments bool) linear.Issue {
	out := *src
	out.Labels = append([]linear.Label(nil), src.Labels...)
	out.InverseRelations = append([]linear.IssueRelation(nil), src.InverseRelations...)
	if src.Assignee != nil {
		assignee := *src.Assignee
		out.Assignee = &assignee
	}
	if src.Parent != nil {
		parent := *src.Parent
		out.Parent = &parent
	}
	if !includeComments {
		out.Comments = nil
	}
	return out
}

func copyComments(src []linear.Comment) []linear.Comment {
	if src == nil {
		return nil
	}
	out := make([]linear.Comment, len(src))
	copy(out, src)
	return out
}

func notFoundError(format string, args ...any) *domain.TrackerError {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerNotFound,
		Message: fmt.Sprintf(format+" not found", args...),
	}
}

func payloadError(format string, args ...any) *domain.TrackerError {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf(format, args...),
	}
}
