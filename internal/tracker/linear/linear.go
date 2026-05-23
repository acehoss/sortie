// Package linear implements [domain.TrackerAdapter] for Linear's
// GraphQL API. Issues are scoped by team key (configured as
// tracker.project); labels are team-scoped and created on-miss during
// AddLabel; state transitions resolve the target stateId via a single
// embedded query (issue → team → states(filter)). Registered under
// kind "linear" via an init function in register.go.
package linear

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// defaultActiveStates is applied when the adapter config omits
// active_states. Matches the common Linear default workflow.
var defaultActiveStates = []string{"Backlog", "Todo"}

// pageSize is the GraphQL connection page size used by all paginated
// queries. Matches the spec's 50-default.
const pageSize = 50

// LinearAdapter implements [domain.TrackerAdapter] against Linear's
// GraphQL API. Safe for concurrent use.
type LinearAdapter struct {
	client       Client
	teamKey      string
	activeStates []string
	assigneeID   string // resolved user UUID; empty means no assignee filter
	metrics      domain.Metrics // nil-safe: check before calling

	// labelCacheMu guards labelCache. The cache is populated lazily
	// per team as AddLabel is called and refreshed when a create
	// returns "already exists" (concurrent-create race recovery).
	labelCacheMu sync.Mutex
	labelCache   map[string]map[string]string // teamID → lowercase(name) → labelID
}

// Compile-time interface satisfaction check.
var _ domain.TrackerAdapter = (*LinearAdapter)(nil)

// newAdapterWithClient constructs a LinearAdapter against the given
// Client. Used by tests with linearmock.Fake and by the public
// constructor with the real GraphQL transport. assigneeID is the
// already-resolved user UUID (the registry resolves "me" via
// QueryViewer before reaching this point); empty means no filter.
func newAdapterWithClient(client Client, teamKey string, activeStates []string, assigneeID string) *LinearAdapter {
	if len(activeStates) == 0 {
		activeStates = defaultActiveStates
	}
	return &LinearAdapter{
		client:       client,
		teamKey:      teamKey,
		activeStates: activeStates,
		assigneeID:   assigneeID,
		labelCache:   map[string]map[string]string{},
	}
}

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter
// operates without recording metrics. Safe to call before any adapter
// operations; not safe to call concurrently with adapter operations.
func (a *LinearAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// extractAdapterConfig pulls the common Linear config fields out of
// the map[string]any passed by the registry. Returns the team key,
// active states, and any validation error.
func extractAdapterConfig(config map[string]any) (teamKey string, activeStates []string, err error) {
	teamKey, _ = config["project"].(string)
	if teamKey == "" {
		return "", nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerProject,
			Message: "missing required config key: project",
		}
	}
	activeStates = typeutil.ExtractStringSlice(config["active_states"])
	if len(activeStates) == 0 {
		activeStates = defaultActiveStates
	}
	return teamKey, activeStates, nil
}

// FetchCandidateIssues returns issues in configured active states for
// the configured team. Comments are nil on returned issues; the caller
// promotes a candidate via FetchIssueByID before dispatch.
func (a *LinearAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	var out []domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		issues, fetchErr := a.paginatedIssues(ctx, IssuesFilter{
			TeamKey:    a.teamKey,
			StateNames: a.activeStates,
			AssigneeID: a.assigneeID,
		})
		if fetchErr != nil {
			return fetchErr
		}
		out = issues
		return nil
	})
	return out, err
}

// FetchIssueByID returns a fully-populated issue including comments.
// The issueID is either the Linear UUID or the human identifier
// (Linear's Query.issue(id) accepts both forms).
func (a *LinearAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var out domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		raw, err := a.client.QueryIssueByKey(ctx, issueID)
		if err != nil {
			return err
		}
		issue := normalizeIssue(raw)
		comments, err := a.paginatedComments(ctx, raw.ID)
		if err != nil {
			return err
		}
		issue.Comments = comments
		out = issue
		return nil
	})
	return out, err
}

// FetchIssuesByStates returns issues in the configured team whose
// state name is in the supplied list. Used for startup terminal
// cleanup. Returns an empty non-nil slice when states is empty.
func (a *LinearAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}
	var out []domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		issues, fetchErr := a.paginatedIssues(ctx, IssuesFilter{
			TeamKey:    a.teamKey,
			StateNames: states,
			AssigneeID: a.assigneeID,
		})
		if fetchErr != nil {
			return fetchErr
		}
		out = issues
		return nil
	})
	return out, err
}

// FetchIssueStatesByIDs returns the current state name for each
// requested issue UUID. Missing issues are omitted from the result.
func (a *LinearAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	var out map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		states, fetchErr := a.client.QueryIssueStatesByKeys(ctx, issueIDs)
		if fetchErr != nil {
			return fetchErr
		}
		out = states
		return nil
	})
	return out, err
}

// FetchIssueStatesByIdentifiers returns the current state name for
// each requested human identifier (e.g. "ENG-123"). Missing issues
// are omitted. Identical wire path to FetchIssueStatesByIDs because
// Linear's Query.issue(id) accepts both UUIDs and identifiers.
func (a *LinearAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	var out map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		states, fetchErr := a.client.QueryIssueStatesByKeys(ctx, identifiers)
		if fetchErr != nil {
			return fetchErr
		}
		out = states
		return nil
	})
	return out, err
}

// FetchIssueComments pages all comments for the issue. Returns an
// empty non-nil slice when no comments exist; returns ErrTrackerNotFound
// when the issue does not exist.
func (a *LinearAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	var out []domain.Comment
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		comments, fetchErr := a.paginatedComments(ctx, issueID)
		if fetchErr != nil {
			return fetchErr
		}
		out = comments
		return nil
	})
	return out, err
}

// TransitionIssue moves an issue to the named target state via a
// single resolve query plus the issueUpdate mutation.
func (a *LinearAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	return trackermetrics.Track(a.metrics, "transition", func() error {
		stateID, err := a.client.QueryStateIDByName(ctx, issueID, targetState)
		if err != nil {
			return err
		}
		return a.client.MutationIssueUpdateState(ctx, issueID, stateID)
	})
}

// CommentIssue posts a markdown comment on the issue. The returned
// comment ID is discarded; the orchestrator does not need it.
func (a *LinearAdapter) CommentIssue(ctx context.Context, issueID string, text string) error {
	return trackermetrics.Track(a.metrics, "comment", func() error {
		_, err := a.client.MutationCommentCreate(ctx, issueID, text)
		return err
	})
}

// AddLabel attaches the named label to the issue. If the label does
// not exist in the issue's team, it is created. If the label is
// already attached, the call is a no-op. Label name matching is
// case-insensitive (Linear's label model is case-insensitive on
// uniqueness).
//
// The flow is non-atomic: resolve team and current labels → check or
// refresh team-label cache → optionally create → apply via issueUpdate
// with the merged labelIds list (Linear's labelIds replaces, not
// appends).
func (a *LinearAdapter) AddLabel(ctx context.Context, issueID string, label string) error {
	return trackermetrics.Track(a.metrics, "add_label", func() error {
		res, err := a.client.QueryIssueLabels(ctx, issueID)
		if err != nil {
			return err
		}

		// Idempotency: already attached.
		for _, lbl := range res.Labels {
			if strings.EqualFold(lbl.Name, label) {
				return nil
			}
		}

		labelID, err := a.resolveOrCreateLabel(ctx, res.TeamID, label)
		if err != nil {
			return err
		}

		merged := make([]string, 0, len(res.Labels)+1)
		for _, lbl := range res.Labels {
			merged = append(merged, lbl.ID)
		}
		merged = append(merged, labelID)
		return a.client.MutationIssueUpdateLabels(ctx, issueID, merged)
	})
}

// ───── internal helpers ───────────────────────────────────────────────

// paginatedIssues drives QueryIssues with the standard 50-page size
// and cursor walk. Returns a normalized [domain.Issue] slice.
func (a *LinearAdapter) paginatedIssues(ctx context.Context, filter IssuesFilter) ([]domain.Issue, error) {
	out := make([]domain.Issue, 0)
	after := ""
	for {
		conn, err := a.client.QueryIssues(ctx, filter, pageSize, after)
		if err != nil {
			return nil, err
		}
		for i := range conn.Nodes {
			out = append(out, normalizeIssue(&conn.Nodes[i]))
		}
		if !conn.PageInfo.HasNextPage {
			return out, nil
		}
		if conn.PageInfo.EndCursor == "" {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "linear pagination: hasNextPage=true with empty endCursor",
			}
		}
		after = conn.PageInfo.EndCursor
	}
}

// paginatedComments drives QueryIssueComments with the standard
// 50-page size and cursor walk. Returns an empty non-nil slice when
// no comments exist.
func (a *LinearAdapter) paginatedComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	out := make([]domain.Comment, 0)
	after := ""
	for {
		conn, err := a.client.QueryIssueComments(ctx, issueID, pageSize, after)
		if err != nil {
			return nil, err
		}
		for _, c := range conn.Nodes {
			out = append(out, normalizeComment(c))
		}
		if !conn.PageInfo.HasNextPage {
			return out, nil
		}
		if conn.PageInfo.EndCursor == "" {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "linear comment pagination: hasNextPage=true with empty endCursor",
			}
		}
		after = conn.PageInfo.EndCursor
	}
}

// resolveOrCreateLabel returns a label UUID for the given team and
// case-insensitive name. The lookup order is: in-memory cache →
// team-labels refresh → create. On a concurrent-create race
// (MutationIssueLabelCreate returns ErrTrackerPayload containing
// "already exists"), refresh once and retry the lookup.
func (a *LinearAdapter) resolveOrCreateLabel(ctx context.Context, teamID, name string) (string, error) {
	if id := a.cachedLabel(teamID, name); id != "" {
		return id, nil
	}
	if err := a.refreshTeamLabels(ctx, teamID); err != nil {
		return "", err
	}
	if id := a.cachedLabel(teamID, name); id != "" {
		return id, nil
	}

	id, err := a.client.MutationIssueLabelCreate(ctx, teamID, name)
	if err == nil {
		a.cacheLabel(teamID, name, id)
		return id, nil
	}

	if !isAlreadyExistsError(err) {
		return "", err
	}

	// Race lost: another writer created the label between our refresh
	// and our create. Refresh once more and look it up.
	if rerr := a.refreshTeamLabels(ctx, teamID); rerr != nil {
		return "", rerr
	}
	if id := a.cachedLabel(teamID, name); id != "" {
		return id, nil
	}
	return "", &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf("label %q reported as existing but not present in team %q after refresh", name, teamID),
	}
}

func (a *LinearAdapter) cachedLabel(teamID, name string) string {
	a.labelCacheMu.Lock()
	defer a.labelCacheMu.Unlock()
	if team, ok := a.labelCache[teamID]; ok {
		return team[strings.ToLower(name)]
	}
	return ""
}

func (a *LinearAdapter) cacheLabel(teamID, name, id string) {
	a.labelCacheMu.Lock()
	defer a.labelCacheMu.Unlock()
	team, ok := a.labelCache[teamID]
	if !ok {
		team = map[string]string{}
		a.labelCache[teamID] = team
	}
	team[strings.ToLower(name)] = id
}

func (a *LinearAdapter) refreshTeamLabels(ctx context.Context, teamID string) error {
	labels, err := a.client.QueryTeamLabels(ctx, teamID)
	if err != nil {
		return err
	}
	a.labelCacheMu.Lock()
	defer a.labelCacheMu.Unlock()
	team := make(map[string]string, len(labels))
	for _, lbl := range labels {
		team[strings.ToLower(lbl.Name)] = lbl.ID
	}
	a.labelCache[teamID] = team
	return nil
}

func isAlreadyExistsError(err error) bool {
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerPayload {
		return false
	}
	return strings.Contains(strings.ToLower(te.Message), "already exists")
}
