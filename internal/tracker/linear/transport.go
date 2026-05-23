package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// defaultEndpoint is the public Linear GraphQL endpoint. Operators can
// override via tracker.endpoint to point at a proxy or test server.
const defaultEndpoint = "https://api.linear.app/graphql"

// defaultTimeout matches the §11.2 30-second network timeout.
const defaultTimeout = 30 * time.Second

// maxBatchSize caps the number of aliased issue(id) selections per
// QueryIssueStatesByKeys request. At ~3 complexity points per item,
// 50 keys cost ~150 points — comfortably below Linear's 10K
// single-query cap.
const maxBatchSize = 50

// teamLabelsPageSize is the upper bound on labels fetched per team.
// Teams with more than this need pagination; Sortie's escalation flow
// does not require it.
const teamLabelsPageSize = 100

// ClientOptions configures the real GraphQL Client.
type ClientOptions struct {
	// Endpoint overrides the default Linear GraphQL endpoint. Use the
	// full URL (e.g. https://api.linear.app/graphql). Empty value
	// resolves to defaultEndpoint.
	Endpoint string

	// APIKey is the Linear personal API key (e.g. "lin_api_...") or
	// OAuth access token. Personal API keys are sent verbatim; OAuth
	// tokens require the caller to set UseBearer = true.
	APIKey string

	// UseBearer prefixes the APIKey with "Bearer " when set. Defaults
	// to false (Linear personal API keys MUST NOT be prefixed with
	// Bearer; OAuth tokens MUST be).
	UseBearer bool

	// UserAgent identifies the client in the request header. Empty
	// resolves to "sortie/dev".
	UserAgent string

	// Timeout overrides the per-request timeout. Zero resolves to
	// defaultTimeout (30s).
	Timeout time.Duration
}

// NewClient constructs the real GraphQL [Client]. Returns an error
// when APIKey is empty.
func NewClient(opts ClientOptions) (Client, error) {
	if opts.APIKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "linear: missing api_key",
		}
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = "sortie/dev"
	}
	authHeader := opts.APIKey
	if opts.UseBearer {
		authHeader = "Bearer " + opts.APIKey
	}

	httpClient := httpkit.NewClient(httpkit.ClientOptions{
		Timeout: timeout,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", authHeader)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: classifyTransportError,
	})
	return &gqlClient{httpClient: httpClient, endpoint: endpoint}, nil
}

// gqlClient is the real [Client] implementation. It speaks raw
// GraphQL over the httpkit HTTP transport.
type gqlClient struct {
	httpClient *httpkit.Client
	endpoint   string
}

// ───── interface methods ──────────────────────────────────────────────

func (c *gqlClient) QueryIssues(ctx context.Context, filter IssuesFilter, first int, after string) (*IssueConnection, error) {
	gqlFilter := map[string]any{
		"team": map[string]any{"key": map[string]any{"eq": filter.TeamKey}},
	}
	if len(filter.StateNames) > 0 {
		gqlFilter["state"] = map[string]any{"name": map[string]any{"in": filter.StateNames}}
	}
	vars := map[string]any{
		"filter":        gqlFilter,
		"first":         first,
		"relationFirst": pageSize,
	}
	if after != "" {
		vars["after"] = after
	}

	var resp struct {
		Issues struct {
			Nodes    []rawIssue  `json:"nodes"`
			PageInfo rawPageInfo `json:"pageInfo"`
		} `json:"issues"`
	}
	if err := c.execute(ctx, queryIssues, vars, &resp); err != nil {
		return nil, err
	}
	conn := &IssueConnection{
		Nodes:    make([]Issue, len(resp.Issues.Nodes)),
		PageInfo: resp.Issues.PageInfo.toDomain(),
	}
	for i := range resp.Issues.Nodes {
		conn.Nodes[i] = resp.Issues.Nodes[i].toDomain()
	}
	return conn, nil
}

func (c *gqlClient) QueryIssueByKey(ctx context.Context, key string) (*Issue, error) {
	vars := map[string]any{"id": key, "relationFirst": pageSize}

	var resp struct {
		Issue *rawIssue `json:"issue"`
	}
	if err := c.execute(ctx, queryIssueByKey, vars, &resp); err != nil {
		return nil, err
	}
	if resp.Issue == nil {
		return nil, notFoundf("issue %q", key)
	}
	out := resp.Issue.toDomain()
	return &out, nil
}

func (c *gqlClient) QueryIssueStatesByKeys(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for start := 0; start < len(keys); start += maxBatchSize {
		end := start + maxBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		if err := c.queryStatesChunk(ctx, chunk, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// queryStatesChunk builds an aliased query for up to maxBatchSize keys
// and merges results into out. Missing issues (data null and/or
// ENTITY_NOT_FOUND in errors) are silently omitted per the spec.
func (c *gqlClient) queryStatesChunk(ctx context.Context, keys []string, out map[string]string) error {
	if len(keys) == 0 {
		return nil
	}

	// Build:
	//   query SortieLinearIssueStates($k0: String!, $k1: String!) {
	//     i0: issue(id: $k0) { id state { name } }
	//     ...
	//   }
	var qb strings.Builder
	qb.WriteString("query SortieLinearIssueStates(")
	for i := range keys {
		if i > 0 {
			qb.WriteString(", ")
		}
		fmt.Fprintf(&qb, "$k%d: String!", i)
	}
	qb.WriteString(") {\n")
	for i := range keys {
		fmt.Fprintf(&qb, "  i%d: issue(id: $k%d) { id state { name } }\n", i, i)
	}
	qb.WriteString("}")

	vars := make(map[string]any, len(keys))
	for i, k := range keys {
		vars[fmt.Sprintf("k%d", i)] = k
	}

	// Decode into a map[alias]rawStateIssue. Aliases not present (null
	// data, ENTITY_NOT_FOUND in errors) are omitted from the result.
	raw := map[string]*struct {
		ID    string `json:"id"`
		State struct {
			Name string `json:"name"`
		} `json:"state"`
	}{}

	if err := c.executePartial(ctx, qb.String(), vars, &raw); err != nil {
		return err
	}
	for i, k := range keys {
		alias := fmt.Sprintf("i%d", i)
		if entry, ok := raw[alias]; ok && entry != nil {
			out[k] = entry.State.Name
		}
	}
	return nil
}

func (c *gqlClient) QueryIssueComments(ctx context.Context, issueID string, first int, after string) (*CommentConnection, error) {
	vars := map[string]any{"id": issueID, "first": first}
	if after != "" {
		vars["after"] = after
	}

	var resp struct {
		Issue *struct {
			Comments struct {
				Nodes    []rawComment `json:"nodes"`
				PageInfo rawPageInfo  `json:"pageInfo"`
			} `json:"comments"`
		} `json:"issue"`
	}
	if err := c.execute(ctx, queryIssueComments, vars, &resp); err != nil {
		return nil, err
	}
	if resp.Issue == nil {
		return nil, notFoundf("issue %q", issueID)
	}
	conn := &CommentConnection{
		Nodes:    make([]Comment, len(resp.Issue.Comments.Nodes)),
		PageInfo: resp.Issue.Comments.PageInfo.toDomain(),
	}
	for i, rc := range resp.Issue.Comments.Nodes {
		conn.Nodes[i] = rc.toDomain()
	}
	return conn, nil
}

func (c *gqlClient) QueryStateIDByName(ctx context.Context, issueID, stateName string) (string, error) {
	vars := map[string]any{"issueId": issueID, "stateName": stateName}

	var resp struct {
		Issue *struct {
			Team struct {
				States struct {
					Nodes []struct {
						ID string `json:"id"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := c.execute(ctx, queryStateIDByName, vars, &resp); err != nil {
		return "", err
	}
	if resp.Issue == nil {
		return "", notFoundf("issue %q", issueID)
	}
	if len(resp.Issue.Team.States.Nodes) == 0 {
		return "", payloadf("no workflow state named %q in issue's team", stateName)
	}
	return resp.Issue.Team.States.Nodes[0].ID, nil
}

func (c *gqlClient) QueryIssueLabels(ctx context.Context, issueID string) (*IssueLabelsResult, error) {
	vars := map[string]any{"id": issueID}

	var resp struct {
		Issue *struct {
			Team struct {
				ID string `json:"id"`
			} `json:"team"`
			Labels struct {
				Nodes []rawLabel `json:"nodes"`
			} `json:"labels"`
		} `json:"issue"`
	}
	if err := c.execute(ctx, queryIssueLabels, vars, &resp); err != nil {
		return nil, err
	}
	if resp.Issue == nil {
		return nil, notFoundf("issue %q", issueID)
	}
	out := &IssueLabelsResult{
		TeamID: resp.Issue.Team.ID,
		Labels: make([]Label, len(resp.Issue.Labels.Nodes)),
	}
	for i, lbl := range resp.Issue.Labels.Nodes {
		out.Labels[i] = Label{ID: lbl.ID, Name: lbl.Name}
	}
	return out, nil
}

func (c *gqlClient) QueryTeamLabels(ctx context.Context, teamID string) ([]Label, error) {
	vars := map[string]any{"teamId": teamID, "first": teamLabelsPageSize}

	var resp struct {
		Team *struct {
			Labels struct {
				Nodes []rawLabel `json:"nodes"`
			} `json:"labels"`
		} `json:"team"`
	}
	if err := c.execute(ctx, queryTeamLabels, vars, &resp); err != nil {
		return nil, err
	}
	if resp.Team == nil {
		return nil, notFoundf("team %q", teamID)
	}
	out := make([]Label, len(resp.Team.Labels.Nodes))
	for i, lbl := range resp.Team.Labels.Nodes {
		out[i] = Label{ID: lbl.ID, Name: lbl.Name}
	}
	return out, nil
}

func (c *gqlClient) MutationIssueUpdateState(ctx context.Context, issueID, stateID string) error {
	vars := map[string]any{"id": issueID, "stateId": stateID}

	var resp struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.execute(ctx, mutationIssueUpdateState, vars, &resp); err != nil {
		return err
	}
	if !resp.IssueUpdate.Success {
		return apif("issueUpdate returned success=false for issue %q", issueID)
	}
	return nil
}

func (c *gqlClient) MutationIssueUpdateLabels(ctx context.Context, issueID string, labelIDs []string) error {
	vars := map[string]any{"id": issueID, "labelIds": labelIDs}

	var resp struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.execute(ctx, mutationIssueUpdateLabels, vars, &resp); err != nil {
		return err
	}
	if !resp.IssueUpdate.Success {
		return apif("issueUpdate labels returned success=false for issue %q", issueID)
	}
	return nil
}

func (c *gqlClient) MutationCommentCreate(ctx context.Context, issueID, body string) (string, error) {
	vars := map[string]any{"issueId": issueID, "body": body}

	var resp struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	if err := c.execute(ctx, mutationCommentCreate, vars, &resp); err != nil {
		return "", err
	}
	if !resp.CommentCreate.Success || resp.CommentCreate.Comment == nil {
		return "", apif("commentCreate returned success=false for issue %q", issueID)
	}
	return resp.CommentCreate.Comment.ID, nil
}

func (c *gqlClient) MutationIssueLabelCreate(ctx context.Context, teamID, name string) (string, error) {
	vars := map[string]any{"teamId": teamID, "name": name}

	var resp struct {
		IssueLabelCreate struct {
			Success    bool `json:"success"`
			IssueLabel *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"issueLabel"`
		} `json:"issueLabelCreate"`
	}
	if err := c.execute(ctx, mutationIssueLabelCreate, vars, &resp); err != nil {
		return "", err
	}
	if !resp.IssueLabelCreate.Success || resp.IssueLabelCreate.IssueLabel == nil {
		return "", apif("issueLabelCreate returned success=false for team %q", teamID)
	}
	return resp.IssueLabelCreate.IssueLabel.ID, nil
}

// ───── internals: execute, errors, raw types ──────────────────────────

// gqlRequest is the JSON request envelope.
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// gqlResponse is the JSON response envelope. data is decoded into the
// caller-supplied destination separately; this struct surfaces only
// the errors array for inspection.
type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// gqlError is one entry in the response's errors array.
type gqlError struct {
	Message    string         `json:"message"`
	Path       []any          `json:"path"`
	Extensions map[string]any `json:"extensions"`
}

// execute runs a query/mutation and decodes data into out. Returns an
// error when the GraphQL errors array is non-empty OR the data field
// is missing/null. Use executePartial when the caller wants to
// tolerate partial-data responses (e.g. aliased-batch lookups).
func (c *gqlClient) execute(ctx context.Context, query string, vars map[string]any, out any) error {
	body, errs, err := c.send(ctx, query, vars)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		return classifyGraphQLErrors(errs)
	}
	if len(body) == 0 || string(body) == "null" {
		return payloadf("linear: empty data field")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return payloadf("linear: failed to parse response: %v", err)
	}
	return nil
}

// executePartial runs a query and decodes data into out even when the
// errors array is non-empty, as long as the only errors are
// ENTITY_NOT_FOUND. Other error codes still fail. Used by the
// aliased-batch state lookup so missing items can be silently omitted.
func (c *gqlClient) executePartial(ctx context.Context, query string, vars map[string]any, out any) error {
	body, errs, err := c.send(ctx, query, vars)
	if err != nil {
		return err
	}
	for _, e := range errs {
		code, _ := e.Extensions["code"].(string)
		if code != "ENTITY_NOT_FOUND" {
			return classifyGraphQLErrors(errs)
		}
	}
	if len(body) == 0 || string(body) == "null" {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return payloadf("linear: failed to parse response: %v", err)
	}
	return nil
}

// send marshals the request, posts it, and returns the response's
// data field plus errors array. Transport-level failures and HTTP
// non-200 responses surface as *domain.TrackerError via the httpkit
// classifier closures.
func (c *gqlClient) send(ctx context.Context, query string, vars map[string]any) (json.RawMessage, []gqlError, error) {
	payload, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return nil, nil, payloadf("linear: failed to marshal request: %v", err)
	}

	respBytes, err := c.httpClient.Send(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		// On HTTP 400 specifically, the body may carry a RATELIMITED
		// or INVALID_INPUT envelope that we want to reclassify; the
		// httpkit error closure already runs, but the body is gone.
		// Acceptable: RATELIMITED is captured as ErrTrackerAPI via the
		// status path; the operator can inspect logs for the reset
		// epoch header (logged by the classifier).
		return nil, nil, err
	}
	var env gqlResponse
	if err := json.Unmarshal(respBytes, &env); err != nil {
		return nil, nil, payloadf("linear: failed to parse response envelope: %v", err)
	}
	return env.Data, env.Errors, nil
}

// classifyGraphQLErrors maps a non-empty errors array to a
// *domain.TrackerError. Picks the most informative single category
// based on extension codes; if multiple errors are present, the first
// recognized code wins.
func classifyGraphQLErrors(errs []gqlError) error {
	if len(errs) == 0 {
		return nil
	}
	first := errs[0]
	code, _ := first.Extensions["code"].(string)
	kind := domain.ErrTrackerAPI
	switch code {
	case "ENTITY_NOT_FOUND":
		kind = domain.ErrTrackerNotFound
	case "AUTHENTICATION_ERROR", "FORBIDDEN":
		kind = domain.ErrTrackerAuth
	case "INVALID_INPUT":
		kind = domain.ErrTrackerPayload
	case "INTERNAL_SERVER_ERROR":
		kind = domain.ErrTrackerTransport
	case "RATELIMITED":
		kind = domain.ErrTrackerAPI
	}
	// Linear's actual API surfaces missing entities under several
	// extension codes (observed: INPUT_ERROR, FEATURE_NOT_ACCESSIBLE),
	// not just ENTITY_NOT_FOUND. Promote any "Entity not found" prefix
	// to NotFound so callers can rely on domain.IsNotFound.
	if kind != domain.ErrTrackerNotFound && strings.HasPrefix(first.Message, "Entity not found") {
		kind = domain.ErrTrackerNotFound
	}
	msg := first.Message
	if msg == "" {
		msg = fmt.Sprintf("linear: graphql error (code=%s)", code)
	} else if code != "" {
		msg = fmt.Sprintf("%s (code=%s)", msg, code)
	}
	return &domain.TrackerError{Kind: kind, Message: msg}
}

// classifyHTTPError maps a non-2xx HTTP response to a tracker error.
// The Linear GraphQL endpoint returns 400 for rate-limit responses,
// 401 for invalid keys, and 5xx for server errors. The response body
// is read up to 512 bytes for diagnostic context.
func classifyHTTPError(resp *http.Response, method, path string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_, _ = io.Copy(io.Discard, resp.Body)
	detail := string(snippet)

	// Headers worth surfacing.
	hdrs := []string{}
	for _, h := range []string{
		"X-RateLimit-Requests-Remaining",
		"X-RateLimit-Complexity-Remaining",
		"X-RateLimit-Requests-Reset",
	} {
		if v := resp.Header.Get(h); v != "" {
			hdrs = append(hdrs, fmt.Sprintf("%s=%s", h, v))
		}
	}
	sort.Strings(hdrs)
	hdrStr := ""
	if len(hdrs) > 0 {
		hdrStr = " [" + strings.Join(hdrs, " ") + "]"
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("linear: 401 unauthorized: %s%s", detail, hdrStr),
		}
	case resp.StatusCode == http.StatusForbidden:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("linear: 403 forbidden: %s%s", detail, hdrStr),
		}
	case resp.StatusCode == http.StatusNotFound:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("linear: 404 not found: %s%s", detail, hdrStr),
		}
	case resp.StatusCode == http.StatusBadRequest:
		kind := domain.ErrTrackerAPI
		if strings.Contains(strings.ToLower(detail), "ratelimited") ||
			strings.Contains(strings.ToLower(detail), "rate limit") {
			kind = domain.ErrTrackerAPI
		}
		return &domain.TrackerError{
			Kind:    kind,
			Message: fmt.Sprintf("linear: 400 bad request: %s%s", detail, hdrStr),
		}
	case resp.StatusCode >= 500:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("linear: %d server error: %s%s", resp.StatusCode, detail, hdrStr),
		}
	default:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("linear: unexpected status %d: %s%s", resp.StatusCode, detail, hdrStr),
		}
	}
}

func classifyTransportError(err error, method, path string) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerTransport,
		Message: fmt.Sprintf("linear: transport error %s %s", method, path),
		Err:     err,
	}
}

func notFoundf(format string, args ...any) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerNotFound,
		Message: fmt.Sprintf(format+" not found", args...),
	}
}

func payloadf(format string, args ...any) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf(format, args...),
	}
}

func apif(format string, args ...any) error {
	return &domain.TrackerError{
		Kind:    domain.ErrTrackerAPI,
		Message: fmt.Sprintf(format, args...),
	}
}

// ───── raw response shapes (private mirror of wire types) ─────────────

// rawIssue is the JSON shape of an Issue node as returned by the GraphQL
// API. It is converted to the exported [Issue] via toDomain. Kept
// separate from [Issue] so adding GraphQL-only intermediate fields
// (connection wrappers) doesn't leak into the public wire type.
type rawIssue struct {
	ID          string        `json:"id"`
	Identifier  string        `json:"identifier"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Priority    float64       `json:"priority"`
	BranchName  string        `json:"branchName"`
	URL         string        `json:"url"`
	CreatedAt   string        `json:"createdAt"`
	UpdatedAt   string        `json:"updatedAt"`
	State       WorkflowState `json:"state"`
	Assignee    *User         `json:"assignee"`
	Parent      *ParentRef    `json:"parent"`
	Team        IssueTeamRef  `json:"team"`
	Labels      struct {
		Nodes []rawLabel `json:"nodes"`
	} `json:"labels"`
	InverseRelations struct {
		Nodes []rawInverseRelation `json:"nodes"`
	} `json:"inverseRelations"`
}

func (r rawIssue) toDomain() Issue {
	labels := make([]Label, len(r.Labels.Nodes))
	for i, lbl := range r.Labels.Nodes {
		labels[i] = Label{ID: lbl.ID, Name: lbl.Name}
	}
	rels := make([]IssueRelation, len(r.InverseRelations.Nodes))
	for i, rel := range r.InverseRelations.Nodes {
		rels[i] = IssueRelation{
			Type: rel.Type,
			Issue: IssueRef{
				ID:         rel.Issue.ID,
				Identifier: rel.Issue.Identifier,
				State:      rel.Issue.State.Name,
			},
		}
	}
	return Issue{
		ID:               r.ID,
		Identifier:       r.Identifier,
		Title:            r.Title,
		Description:      r.Description,
		Priority:         r.Priority,
		BranchName:       r.BranchName,
		URL:              r.URL,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
		State:            r.State,
		Assignee:         r.Assignee,
		Parent:           r.Parent,
		Team:             r.Team,
		Labels:           labels,
		InverseRelations: rels,
	}
}

type rawLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type rawInverseRelation struct {
	Type  string `json:"type"`
	Issue struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		State      struct {
			Name string `json:"name"`
		} `json:"state"`
	} `json:"issue"`
}

type rawComment struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	User      *User  `json:"user"`
}

func (r rawComment) toDomain() Comment {
	return Comment{
		ID:        r.ID,
		Body:      r.Body,
		CreatedAt: r.CreatedAt,
		User:      r.User,
	}
}

type rawPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

func (p rawPageInfo) toDomain() PageInfo {
	return PageInfo{HasNextPage: p.HasNextPage, EndCursor: p.EndCursor}
}
