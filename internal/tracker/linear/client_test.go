package linear_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/tracker/linear"
)

// gqlReq is what we expect every Linear request to look like.
type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// gqlHandler is a test helper that decodes the incoming GraphQL
// request, dispatches by operation-name heuristic (first non-empty
// line of the query containing the operation type+name), and writes
// the per-operation response. Unhandled operations return HTTP 500
// with a debug message so the test fails loudly.
type gqlHandler func(t *testing.T, req gqlReq) (status int, header http.Header, body any)

// newTestClient spins up an httptest.Server, returns a real Client
// wired to it, and a cleanup func.
func newTestClient(t *testing.T, h gqlHandler) linear.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("Authorization"); got != "lin_api_test" {
			t.Errorf("Authorization header = %q, want %q (raw key, no Bearer)", got, "lin_api_test")
		}
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var req gqlReq
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			t.Errorf("decode request: %v; body=%s", err, string(bodyBytes))
			return
		}
		status, hdrs, payload := h(t, req)
		for k, vs := range hdrs {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)

	client, err := linear.NewClient(linear.ClientOptions{
		Endpoint:  srv.URL,
		APIKey:    "lin_api_test",
		UserAgent: "sortie-test/1.0",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestQueryIssues_HappyPath(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		if !strings.Contains(req.Query, "issues(filter:") {
			t.Errorf("query does not look like an issues query: %s", req.Query)
		}
		filter, _ := req.Variables["filter"].(map[string]any)
		team, _ := filter["team"].(map[string]any)
		teamKey, _ := team["key"].(map[string]any)
		if got := teamKey["eq"]; got != "ENG" {
			t.Errorf("filter.team.key.eq = %v, want ENG", got)
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes": []map[string]any{
						{
							"id":          "iss-1",
							"identifier":  "ENG-1",
							"title":       "first",
							"description": "body",
							"priority":    2.0,
							"branchName":  "eng/eng-1",
							"url":         "https://linear.app/example/issue/ENG-1",
							"createdAt":   "2026-01-01T00:00:00Z",
							"updatedAt":   "2026-01-01T00:00:00Z",
							"state":       map[string]any{"id": "ws-backlog", "name": "Backlog", "type": "backlog"},
							"assignee":    map[string]any{"displayName": "Alice"},
							"team":        map[string]any{"id": "team-eng"},
							"labels":      map[string]any{"nodes": []map[string]any{{"id": "lbl-bug", "name": "Bug"}}},
							"inverseRelations": map[string]any{
								"nodes": []map[string]any{
									{"type": "blocks", "issue": map[string]any{"id": "iss-2", "identifier": "ENG-2", "state": map[string]any{"name": "Todo"}}},
								},
							},
						},
					},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				},
			},
		}
	})

	conn, err := client.QueryIssues(context.Background(), linear.IssuesFilter{
		TeamKey:    "ENG",
		StateNames: []string{"Backlog", "Todo"},
	}, 50, "")
	if err != nil {
		t.Fatalf("QueryIssues: %v", err)
	}
	if len(conn.Nodes) != 1 {
		t.Fatalf("len(Nodes) = %d, want 1", len(conn.Nodes))
	}
	iss := conn.Nodes[0]
	if iss.Identifier != "ENG-1" || iss.State.Name != "Backlog" {
		t.Errorf("issue = %+v", iss)
	}
	if iss.Assignee == nil || iss.Assignee.DisplayName != "Alice" {
		t.Errorf("assignee = %+v", iss.Assignee)
	}
	if len(iss.Labels) != 1 || iss.Labels[0].Name != "Bug" {
		t.Errorf("labels = %+v", iss.Labels)
	}
	if len(iss.InverseRelations) != 1 || iss.InverseRelations[0].Type != "blocks" {
		t.Errorf("relations = %+v", iss.InverseRelations)
	}
}

func TestQueryIssues_GraphQLErrorClassified(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"errors": []map[string]any{
				{
					"message":    "Authentication required",
					"extensions": map[string]any{"code": "AUTHENTICATION_ERROR"},
				},
			},
		}
	})

	_, err := client.QueryIssues(context.Background(), linear.IssuesFilter{TeamKey: "ENG"}, 50, "")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerAuth {
		t.Errorf("err = %v, want ErrTrackerAuth", err)
	}
}

func TestQueryIssues_RateLimitedFromHTTP400(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		hdr := http.Header{}
		hdr.Set("X-RateLimit-Requests-Remaining", "0")
		hdr.Set("X-RateLimit-Requests-Reset", "1716470000000")
		return 400, hdr, map[string]any{
			"errors": []map[string]any{
				{
					"message":    "Rate limited",
					"extensions": map[string]any{"code": "RATELIMITED"},
				},
			},
		}
	})

	_, err := client.QueryIssues(context.Background(), linear.IssuesFilter{TeamKey: "ENG"}, 50, "")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerAPI {
		t.Errorf("err = %v, want ErrTrackerAPI", err)
	}
	if !strings.Contains(te.Message, "X-RateLimit-Requests-Reset") {
		t.Errorf("error message missing rate limit headers: %s", te.Message)
	}
}

func TestQueryIssueByKey_HappyPath(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		if got := req.Variables["id"]; got != "ENG-1" {
			t.Errorf("id var = %v, want ENG-1", got)
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id":               "iss-1",
					"identifier":       "ENG-1",
					"title":            "single",
					"priority":         1.0,
					"state":            map[string]any{"id": "ws-todo", "name": "Todo", "type": "unstarted"},
					"team":             map[string]any{"id": "team-eng"},
					"labels":           map[string]any{"nodes": []any{}},
					"inverseRelations": map[string]any{"nodes": []any{}},
				},
			},
		}
	})

	iss, err := client.QueryIssueByKey(context.Background(), "ENG-1")
	if err != nil {
		t.Fatalf("QueryIssueByKey: %v", err)
	}
	if iss.ID != "iss-1" || iss.Identifier != "ENG-1" {
		t.Errorf("issue = %+v", iss)
	}
}

func TestQueryIssueByKey_NullDataReturnsNotFound(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"data": map[string]any{"issue": nil},
		}
	})

	_, err := client.QueryIssueByKey(context.Background(), "ENG-999")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("err = %v, want ErrTrackerNotFound", err)
	}
}

func TestQueryIssueByKey_EntityNotFoundMessageReturnsNotFound(t *testing.T) {
	t.Parallel()
	// Linear's live API returns "Entity not found: Issue" under
	// extension code INPUT_ERROR (not ENTITY_NOT_FOUND as the docs
	// suggest). The classifier must promote any "Entity not found"
	// prefix to ErrTrackerNotFound regardless of extension code.
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"errors": []map[string]any{
				{
					"message":    "Entity not found: Issue",
					"extensions": map[string]any{"code": "INPUT_ERROR"},
				},
			},
		}
	})

	_, err := client.QueryIssueByKey(context.Background(), "ENG-99999999")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("err = %v, want ErrTrackerNotFound", err)
	}
}

func TestQueryIssueStatesByKeys_BuildsAliasedQueryAndMergesResults(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		// Expect three aliased selections.
		for i := 0; i < 3; i++ {
			needle := "i" + itoa(i) + ":"
			if !strings.Contains(req.Query, needle) {
				t.Errorf("query missing alias %q: %s", needle, req.Query)
			}
		}
		// k1 (ENG-MISSING) returns null + ENTITY_NOT_FOUND.
		return 200, nil, map[string]any{
			"data": map[string]any{
				"i0": map[string]any{"id": "iss-1", "state": map[string]any{"name": "Backlog"}},
				"i1": nil,
				"i2": map[string]any{"id": "iss-3", "state": map[string]any{"name": "In Progress"}},
			},
			"errors": []map[string]any{
				{
					"message":    "Entity not found",
					"path":       []any{"i1"},
					"extensions": map[string]any{"code": "ENTITY_NOT_FOUND"},
				},
			},
		}
	})

	got, err := client.QueryIssueStatesByKeys(context.Background(),
		[]string{"iss-1", "ENG-MISSING", "iss-3"})
	if err != nil {
		t.Fatalf("QueryIssueStatesByKeys: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2 (missing omitted)", len(got))
	}
	if got["iss-1"] != "Backlog" || got["iss-3"] != "In Progress" {
		t.Errorf("got = %+v", got)
	}
}

func TestQueryIssueStatesByKeys_NonNotFoundErrorFails(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"errors": []map[string]any{
				{
					"message":    "Forbidden",
					"extensions": map[string]any{"code": "FORBIDDEN"},
				},
			},
		}
	})

	_, err := client.QueryIssueStatesByKeys(context.Background(), []string{"iss-1"})
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerAuth {
		t.Errorf("err = %v, want ErrTrackerAuth", err)
	}
}

func TestQueryStateIDByName_ResolvesUUID(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		if got := req.Variables["stateName"]; got != "In Review" {
			t.Errorf("stateName var = %v, want In Review", got)
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id": "iss-1",
					"team": map[string]any{
						"states": map[string]any{
							"nodes": []map[string]any{{"id": "ws-in-review"}},
						},
					},
				},
			},
		}
	})

	id, err := client.QueryStateIDByName(context.Background(), "iss-1", "In Review")
	if err != nil {
		t.Fatalf("QueryStateIDByName: %v", err)
	}
	if id != "ws-in-review" {
		t.Errorf("id = %q, want ws-in-review", id)
	}
}

func TestQueryStateIDByName_EmptyNodesReturnsPayloadError(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id":   "iss-1",
					"team": map[string]any{"states": map[string]any{"nodes": []any{}}},
				},
			},
		}
	})

	_, err := client.QueryStateIDByName(context.Background(), "iss-1", "Frobnicating")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerPayload {
		t.Errorf("err = %v, want ErrTrackerPayload", err)
	}
}

func TestMutationIssueUpdateState_SuccessTrue(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issueUpdate": map[string]any{
					"success": true,
				},
			},
		}
	})

	if err := client.MutationIssueUpdateState(context.Background(), "iss-1", "ws-in-progress"); err != nil {
		t.Errorf("MutationIssueUpdateState: %v", err)
	}
}

func TestMutationIssueUpdateState_SuccessFalseIsAPIError(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"data": map[string]any{"issueUpdate": map[string]any{"success": false}},
		}
	})

	err := client.MutationIssueUpdateState(context.Background(), "iss-1", "ws-in-progress")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerAPI {
		t.Errorf("err = %v, want ErrTrackerAPI", err)
	}
}

func TestMutationCommentCreate_ReturnsID(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		if got := req.Variables["body"]; got != "from adapter" {
			t.Errorf("body var = %v", got)
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"commentCreate": map[string]any{
					"success": true,
					"comment": map[string]any{"id": "c-42"},
				},
			},
		}
	})

	id, err := client.MutationCommentCreate(context.Background(), "iss-1", "from adapter")
	if err != nil {
		t.Fatalf("MutationCommentCreate: %v", err)
	}
	if id != "c-42" {
		t.Errorf("comment id = %q, want c-42", id)
	}
}

func TestMutationIssueLabelCreate_AlreadyExistsClassifiedAsPayload(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"errors": []map[string]any{
				{
					"message":    "Label \"needs-human\" already exists in team",
					"extensions": map[string]any{"code": "INVALID_INPUT"},
				},
			},
		}
	})

	_, err := client.MutationIssueLabelCreate(context.Background(), "team-eng", "needs-human")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerPayload {
		t.Errorf("err = %v, want ErrTrackerPayload", err)
	}
	if !strings.Contains(strings.ToLower(te.Message), "already exists") {
		t.Errorf("error message does not preserve 'already exists' substring for race detection: %s", te.Message)
	}
}

func TestNewClient_MissingAPIKeyReturnsAuthError(t *testing.T) {
	t.Parallel()
	_, err := linear.NewClient(linear.ClientOptions{})
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrMissingTrackerAPIKey {
		t.Errorf("err = %v, want ErrMissingTrackerAPIKey", err)
	}
}

func TestNewClient_UseBearerAddsBearerPrefix(t *testing.T) {
	t.Parallel()
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id":               "iss-1",
					"identifier":       "ENG-1",
					"state":            map[string]any{"name": "Backlog"},
					"team":             map[string]any{"id": "team-eng"},
					"labels":           map[string]any{"nodes": []any{}},
					"inverseRelations": map[string]any{"nodes": []any{}},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	client, err := linear.NewClient(linear.ClientOptions{
		Endpoint:  srv.URL,
		APIKey:    "oauth_token_xyz",
		UseBearer: true,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.QueryIssueByKey(context.Background(), "ENG-1"); err != nil {
		t.Fatalf("QueryIssueByKey: %v", err)
	}
	if gotAuth != "Bearer oauth_token_xyz" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer oauth_token_xyz")
	}
}

func TestClient_HTTP500ClassifiedAsTransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server fell over", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client, err := linear.NewClient(linear.ClientOptions{
		Endpoint: srv.URL,
		APIKey:   "lin_api_test",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.QueryIssueByKey(context.Background(), "ENG-1")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerTransport {
		t.Errorf("err = %v, want ErrTrackerTransport", err)
	}
}

func TestClient_MalformedJSONReturnsPayloadError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not json"))
	}))
	t.Cleanup(srv.Close)

	client, err := linear.NewClient(linear.ClientOptions{
		Endpoint: srv.URL,
		APIKey:   "lin_api_test",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.QueryIssueByKey(context.Background(), "ENG-1")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerPayload {
		t.Errorf("err = %v, want ErrTrackerPayload", err)
	}
}

func TestQueryIssueComments_PagesAndPopulatesAuthor(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id": "iss-1",
					"comments": map[string]any{
						"nodes": []map[string]any{
							{
								"id":        "c-1",
								"body":      "first",
								"createdAt": "2026-01-01T00:00:00Z",
								"user":      map[string]any{"displayName": "Alice"},
							},
							{
								"id":        "c-2",
								"body":      "second from bot",
								"createdAt": "2026-01-01T00:01:00Z",
								"user":      nil,
							},
						},
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
					},
				},
			},
		}
	})

	conn, err := client.QueryIssueComments(context.Background(), "iss-1", 50, "")
	if err != nil {
		t.Fatalf("QueryIssueComments: %v", err)
	}
	if len(conn.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2", len(conn.Nodes))
	}
	if conn.Nodes[0].User == nil || conn.Nodes[0].User.DisplayName != "Alice" {
		t.Errorf("first comment user = %+v", conn.Nodes[0].User)
	}
	if conn.Nodes[1].User != nil {
		t.Errorf("bot comment user should be nil, got %+v", conn.Nodes[1].User)
	}
}

func TestQueryIssueComments_NullIssueReturnsNotFound(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{"data": map[string]any{"issue": nil}}
	})

	_, err := client.QueryIssueComments(context.Background(), "iss-missing", 50, "")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("err = %v, want ErrTrackerNotFound", err)
	}
}

func TestQueryIssueLabels_HappyPath(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issue": map[string]any{
					"id":   "iss-1",
					"team": map[string]any{"id": "team-eng"},
					"labels": map[string]any{
						"nodes": []map[string]any{
							{"id": "lbl-bug", "name": "Bug"},
						},
					},
				},
			},
		}
	})

	res, err := client.QueryIssueLabels(context.Background(), "iss-1")
	if err != nil {
		t.Fatalf("QueryIssueLabels: %v", err)
	}
	if res.TeamID != "team-eng" {
		t.Errorf("TeamID = %q, want team-eng", res.TeamID)
	}
	if len(res.Labels) != 1 || res.Labels[0].Name != "Bug" {
		t.Errorf("labels = %+v", res.Labels)
	}
}

func TestQueryIssueLabels_WalksAllPages(t *testing.T) {
	t.Parallel()
	// MutationIssueUpdateLabels replaces the label set, so dropping
	// any label past page 1 would silently wipe it. Verify the client
	// walks every page and the caller's "after" cursor is propagated.
	var calls int32
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			if got := req.Variables["after"]; got != nil {
				t.Errorf("page 1 after = %v, want nil", got)
			}
			return 200, nil, map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"id":   "iss-1",
						"team": map[string]any{"id": "team-eng"},
						"labels": map[string]any{
							"nodes": []map[string]any{
								{"id": "lbl-1", "name": "L1"},
								{"id": "lbl-2", "name": "L2"},
							},
							"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cursor-1"},
						},
					},
				},
			}
		case 2:
			if got := req.Variables["after"]; got != "cursor-1" {
				t.Errorf("page 2 after = %v, want cursor-1", got)
			}
			return 200, nil, map[string]any{
				"data": map[string]any{
					"issue": map[string]any{
						"id":   "iss-1",
						"team": map[string]any{"id": "team-eng"},
						"labels": map[string]any{
							"nodes": []map[string]any{
								{"id": "lbl-3", "name": "L3"},
							},
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			}
		default:
			t.Fatalf("unexpected call %d", n)
			return 0, nil, nil
		}
	})

	res, err := client.QueryIssueLabels(context.Background(), "iss-1")
	if err != nil {
		t.Fatalf("QueryIssueLabels: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("call count = %d, want 2", got)
	}
	if len(res.Labels) != 3 {
		t.Fatalf("labels = %+v, want 3", res.Labels)
	}
	for i, want := range []string{"lbl-1", "lbl-2", "lbl-3"} {
		if res.Labels[i].ID != want {
			t.Errorf("labels[%d].ID = %q, want %q", i, res.Labels[i].ID, want)
		}
	}
}

func TestQueryTeamLabels_HappyPath(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		if got := req.Variables["teamId"]; got != "team-eng" {
			t.Errorf("teamId var = %v", got)
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"team": map[string]any{
					"id": "team-eng",
					"labels": map[string]any{
						"nodes": []map[string]any{
							{"id": "lbl-bug", "name": "bug"},
							{"id": "lbl-needs-human", "name": "needs-human"},
						},
					},
				},
			},
		}
	})

	labels, err := client.QueryTeamLabels(context.Background(), "team-eng")
	if err != nil {
		t.Fatalf("QueryTeamLabels: %v", err)
	}
	if len(labels) != 2 {
		t.Fatalf("len(labels) = %d, want 2", len(labels))
	}
	if labels[0].ID != "lbl-bug" || labels[1].Name != "needs-human" {
		t.Errorf("labels = %+v", labels)
	}
}

func TestMutationIssueUpdateLabels_HappyPath(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		ids, _ := req.Variables["labelIds"].([]any)
		if len(ids) != 2 {
			t.Errorf("labelIds len = %d, want 2", len(ids))
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issueUpdate": map[string]any{"success": true},
			},
		}
	})

	if err := client.MutationIssueUpdateLabels(context.Background(), "iss-1",
		[]string{"lbl-bug", "lbl-needs-human"}); err != nil {
		t.Errorf("MutationIssueUpdateLabels: %v", err)
	}
}

func TestQueryViewer_ReturnsAuthenticatedUserID(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		if !strings.Contains(req.Query, "viewer { id }") {
			t.Errorf("query missing viewer selection: %s", req.Query)
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"viewer": map[string]any{"id": "user-uuid-123"},
			},
		}
	})

	got, err := client.QueryViewer(context.Background())
	if err != nil {
		t.Fatalf("QueryViewer: %v", err)
	}
	if got != "user-uuid-123" {
		t.Errorf("got = %q, want user-uuid-123", got)
	}
}

func TestQueryViewer_NullViewerReturnsPayloadError(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		return 200, nil, map[string]any{
			"data": map[string]any{"viewer": nil},
		}
	})

	_, err := client.QueryViewer(context.Background())
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerPayload {
		t.Errorf("err = %v, want ErrTrackerPayload", err)
	}
}

func TestQueryIssues_AssigneeFilterForwardedToServer(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, func(t *testing.T, req gqlReq) (int, http.Header, any) {
		// Sanity: the assignee filter must reach the GraphQL filter
		// object so Linear's server-side filter can apply it.
		filter, _ := req.Variables["filter"].(map[string]any)
		assignee, _ := filter["assignee"].(map[string]any)
		idFilter, _ := assignee["id"].(map[string]any)
		if got := idFilter["eq"]; got != "user-me" {
			t.Errorf("filter.assignee.id.eq = %v, want user-me", got)
		}
		return 200, nil, map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				},
			},
		}
	})

	_, err := client.QueryIssues(context.Background(), linear.IssuesFilter{
		TeamKey:    "ENG",
		AssigneeID: "user-me",
	}, 50, "")
	if err != nil {
		t.Fatalf("QueryIssues: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := []byte{}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
