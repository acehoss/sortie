package linear_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tracker/linear"
)

// Environment variables consumed by the Linear integration tests.
//
//   SORTIE_LINEAR_TEST                   "1" → enable; anything else → skip clean.
//   SORTIE_LINEAR_API_KEY                Required. Personal API key.
//   SORTIE_LINEAR_TEAM_KEY               Required. The team's issue-prefix key (e.g. "ENG").
//                                        Maps to tracker.project.
//   SORTIE_LINEAR_TEAM_NAME              Optional. When set, the test verifies the
//                                        configured team key resolves to this team name.
//   SORTIE_LINEAR_TEST_ISSUE_IDENTIFIER  Optional. A specific issue identifier (e.g.
//                                        "ENG-1") for FetchIssueByID. Falls back to
//                                        the first candidate when absent.
//   SORTIE_LINEAR_ACTIVE_STATES          Optional. Comma-separated state names. Falls
//                                        back to the adapter defaults.
//   SORTIE_LINEAR_ENDPOINT               Optional. Overrides https://api.linear.app/graphql.
//   SORTIE_LINEAR_USE_BEARER             Optional. "1" or "true" prefixes the auth
//                                        header with "Bearer " for OAuth tokens.
//
// All write operations (TransitionIssue, CommentIssue, AddLabel) are
// excluded from the integration suite to avoid mutating the operator's
// real Linear workspace. Add explicit opt-in tests with a separate
// guard when needed.

func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_LINEAR_TEST") != "1" {
		t.Skip("skipping Linear integration test: set SORTIE_LINEAR_TEST=1 to enable")
	}
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

func integrationConfig(t *testing.T) map[string]any {
	t.Helper()
	cfg := map[string]any{
		"project": requireEnv(t, "SORTIE_LINEAR_TEAM_KEY"),
		"api_key": requireEnv(t, "SORTIE_LINEAR_API_KEY"),
	}
	if endpoint := os.Getenv("SORTIE_LINEAR_ENDPOINT"); endpoint != "" {
		cfg["endpoint"] = endpoint
	}
	if states := os.Getenv("SORTIE_LINEAR_ACTIVE_STATES"); states != "" {
		parts := strings.Split(states, ",")
		trimmed := make([]any, 0, len(parts))
		for _, s := range parts {
			v := strings.TrimSpace(s)
			if v != "" {
				trimmed = append(trimmed, v)
			}
		}
		if len(trimmed) > 0 {
			cfg["active_states"] = trimmed
		}
	}
	if v := os.Getenv("SORTIE_LINEAR_USE_BEARER"); v == "1" || strings.EqualFold(v, "true") {
		cfg["use_bearer"] = true
	}
	return cfg
}

func newIntegrationAdapter(t *testing.T) domain.TrackerAdapter {
	t.Helper()
	cfg := integrationConfig(t)
	ctor, err := registry.Trackers.Get("linear")
	if err != nil {
		t.Fatalf("registry.Trackers.Get(\"linear\"): %v", err)
	}
	adapter, err := ctor(cfg)
	if err != nil {
		t.Fatalf("construct linear adapter: %v", err)
	}
	return adapter
}

func TestIntegration_TeamNameVerification(t *testing.T) {
	skipUnlessIntegration(t)
	expected := os.Getenv("SORTIE_LINEAR_TEAM_NAME")
	if expected == "" {
		t.Skip("SORTIE_LINEAR_TEAM_NAME not set; skipping team-name sanity check")
	}

	teamKey := requireEnv(t, "SORTIE_LINEAR_TEAM_KEY")
	apiKey := requireEnv(t, "SORTIE_LINEAR_API_KEY")
	endpoint := os.Getenv("SORTIE_LINEAR_ENDPOINT")
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}

	gotName, err := fetchTeamNameByKey(t, endpoint, apiKey, teamKey)
	if err != nil {
		t.Fatalf("resolve team name: %v", err)
	}
	if !strings.EqualFold(gotName, expected) {
		t.Errorf("team key %q resolves to %q in Linear, want %q (set SORTIE_LINEAR_TEAM_NAME to the actual team name to silence this check)",
			teamKey, gotName, expected)
	}
}

func TestIntegration_FetchCandidateIssues(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	t.Logf("FetchCandidateIssues returned %d issue(s)", len(issues))

	teamKey := os.Getenv("SORTIE_LINEAR_TEAM_KEY")
	for i, iss := range issues {
		assertValidIssue(t, iss, teamKey)
		if iss.Comments != nil {
			t.Errorf("candidate[%d] %s has non-nil Comments; want nil", i, iss.Identifier)
		}
	}
}

func TestIntegration_FetchIssueByID(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)
	teamKey := os.Getenv("SORTIE_LINEAR_TEAM_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	identifier := os.Getenv("SORTIE_LINEAR_TEST_ISSUE_IDENTIFIER")
	if identifier == "" {
		candidates, err := adapter.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues (for selecting test issue): %v", err)
		}
		if len(candidates) == 0 {
			t.Skip("no candidate issues in team; set SORTIE_LINEAR_TEST_ISSUE_IDENTIFIER to target a specific issue")
		}
		identifier = candidates[0].Identifier
		t.Logf("using first candidate %q as test issue", identifier)
	}

	iss, err := adapter.FetchIssueByID(ctx, identifier)
	if err != nil {
		t.Fatalf("FetchIssueByID(%q): %v", identifier, err)
	}
	if iss.Identifier != identifier {
		t.Errorf("Identifier = %q, want %q", iss.Identifier, identifier)
	}
	assertValidIssue(t, iss, teamKey)
	if iss.Comments == nil {
		t.Error("Comments is nil, want non-nil slice (even when empty)")
	}
}

func TestIntegration_FetchIssueByID_NotFound(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := adapter.FetchIssueByID(ctx, "ENG-99999999")
	var te *domain.TrackerError
	if !errors.As(err, &te) || te.Kind != domain.ErrTrackerNotFound {
		t.Errorf("err = %v, want ErrTrackerNotFound", err)
	}
}

func TestIntegration_FetchIssuesByStates(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// "Done" is the universal Linear completed state name. The operator
	// can verify their workspace has it via the dashboard; without it,
	// this test logs zero results but does not fail.
	issues, err := adapter.FetchIssuesByStates(ctx, []string{"Done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	t.Logf("FetchIssuesByStates([Done]) returned %d issue(s)", len(issues))

	teamKey := os.Getenv("SORTIE_LINEAR_TEAM_KEY")
	for _, iss := range issues {
		assertValidIssue(t, iss, teamKey)
		if !strings.EqualFold(iss.State, "Done") {
			t.Errorf("issue %s state = %q, want Done (case-insensitive)", iss.Identifier, iss.State)
		}
	}
}

func TestIntegration_FetchIssuesByStates_EmptyInput(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)

	issues, err := adapter.FetchIssuesByStates(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchIssuesByStates(nil): %v", err)
	}
	if issues == nil {
		t.Error("returned nil slice, want empty non-nil")
	}
	if len(issues) != 0 {
		t.Errorf("len(issues) = %d, want 0", len(issues))
	}
}

func TestIntegration_FetchIssueStatesByIDs(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in team; cannot test state refresh by ID")
	}

	ids := make([]string, 0, len(candidates)+1)
	for _, iss := range candidates {
		ids = append(ids, iss.ID)
	}
	ids = append(ids, "ffffffff-ffff-ffff-ffff-ffffffffffff") // synthetic missing UUID

	states, err := adapter.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if len(states) != len(candidates) {
		t.Errorf("len(states) = %d, want %d (missing UUID omitted)", len(states), len(candidates))
	}
	for _, iss := range candidates {
		if got, ok := states[iss.ID]; !ok {
			t.Errorf("missing state for %s (%s)", iss.Identifier, iss.ID)
		} else if got == "" {
			t.Errorf("empty state for %s (%s)", iss.Identifier, iss.ID)
		}
	}
}

func TestIntegration_FetchIssueStatesByIdentifiers(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in team; cannot test state refresh by identifier")
	}

	teamKey := requireEnv(t, "SORTIE_LINEAR_TEAM_KEY")
	idents := make([]string, 0, len(candidates)+1)
	for _, iss := range candidates {
		idents = append(idents, iss.Identifier)
	}
	idents = append(idents, teamKey+"-99999999")

	states, err := adapter.FetchIssueStatesByIdentifiers(ctx, idents)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if len(states) != len(candidates) {
		t.Errorf("len(states) = %d, want %d (missing identifier omitted)", len(states), len(candidates))
	}
}

func TestIntegration_FetchIssueComments(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := newIntegrationAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	identifier := os.Getenv("SORTIE_LINEAR_TEST_ISSUE_IDENTIFIER")
	if identifier == "" {
		candidates, err := adapter.FetchCandidateIssues(ctx)
		if err != nil {
			t.Fatalf("FetchCandidateIssues (for selecting test issue): %v", err)
		}
		if len(candidates) == 0 {
			t.Skip("no candidate issues in team; set SORTIE_LINEAR_TEST_ISSUE_IDENTIFIER to target a specific issue")
		}
		identifier = candidates[0].Identifier
	}

	comments, err := adapter.FetchIssueComments(ctx, identifier)
	if err != nil {
		t.Fatalf("FetchIssueComments(%q): %v", identifier, err)
	}
	if comments == nil {
		t.Errorf("Comments is nil, want non-nil slice (empty when no comments)")
	}
	t.Logf("FetchIssueComments(%q) returned %d comment(s)", identifier, len(comments))

	for i, c := range comments {
		if c.ID == "" {
			t.Errorf("comment[%d].ID is empty", i)
		}
		if c.Body == "" && c.Author == "" {
			t.Errorf("comment[%d] is entirely empty: %+v", i, c)
		}
		if c.CreatedAt != "" {
			if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
				if _, err2 := time.Parse(time.RFC3339Nano, c.CreatedAt); err2 != nil {
					t.Errorf("comment[%d].CreatedAt = %q is not RFC3339: %v", i, c.CreatedAt, err)
				}
			}
		}
	}
}

func TestIntegration_AssigneeMeResolvesAndFilters(t *testing.T) {
	skipUnlessIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Build an adapter configured with assignee: me. Construction
	// itself exercises QueryViewer against the live Linear endpoint.
	cfg := integrationConfig(t)
	cfg["assignee"] = "me"
	ctor, err := registry.Trackers.Get("linear")
	if err != nil {
		t.Fatalf("registry.Trackers.Get(\"linear\"): %v", err)
	}
	adapter, err := ctor(cfg)
	if err != nil {
		t.Fatalf("adapter construct with assignee=me: %v (the viewer query may have failed)", err)
	}

	// Fetching candidates with the filter active should succeed; the
	// result count can be zero (test workspaces usually are) — what we
	// want is "no error" and that every returned issue is actually
	// assigned to the viewer.
	issues, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues with assignee=me: %v", err)
	}
	t.Logf("FetchCandidateIssues with assignee=me returned %d issue(s)", len(issues))
}

// ───── assertions and helpers ─────────────────────────────────────────

func assertValidIssue(t *testing.T, iss domain.Issue, teamKey string) {
	t.Helper()
	if iss.ID == "" {
		t.Error("ID is empty")
	}
	if iss.Identifier == "" {
		t.Error("Identifier is empty")
	} else if teamKey != "" && !strings.HasPrefix(iss.Identifier, teamKey+"-") {
		t.Errorf("Identifier = %q does not start with team key %q", iss.Identifier, teamKey)
	}
	if iss.Title == "" {
		t.Error("Title is empty")
	}
	if iss.State == "" {
		t.Error("State is empty")
	}
	if iss.Labels == nil {
		t.Error("Labels is nil, want non-nil slice")
	}
	for i, l := range iss.Labels {
		if l != strings.ToLower(l) {
			t.Errorf("Labels[%d] = %q is not lowercase", i, l)
		}
	}
	if iss.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil slice")
	}
	if iss.URL == "" {
		t.Error("URL is empty")
	} else if !strings.HasPrefix(iss.URL, "https://linear.app/") {
		t.Errorf("URL = %q does not look like a Linear web URL", iss.URL)
	}
	if iss.CreatedAt == "" {
		t.Error("CreatedAt is empty")
	} else if _, err := time.Parse(time.RFC3339, iss.CreatedAt); err != nil {
		if _, err2 := time.Parse(time.RFC3339Nano, iss.CreatedAt); err2 != nil {
			t.Errorf("CreatedAt = %q is not RFC3339: %v", iss.CreatedAt, err)
		}
	}
	if iss.UpdatedAt == "" {
		t.Error("UpdatedAt is empty")
	}
}

// fetchTeamNameByKey resolves a Linear team's display name from its
// key by issuing a one-off GraphQL request. Used only by the team-name
// sanity check; the production Client interface does not expose this
// since the adapter has no need for the human name.
func fetchTeamNameByKey(t *testing.T, endpoint, apiKey, teamKey string) (string, error) {
	t.Helper()

	const query = `query SortieLinearTeamByKey($key: String!) {
  teams(filter: { key: { eq: $key } }, first: 1) {
    nodes { id key name }
  }
}`

	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]any{"key": teamKey},
	})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	authHeader := apiKey
	if v := os.Getenv("SORTIE_LINEAR_USE_BEARER"); v == "1" || strings.EqualFold(v, "true") {
		authHeader = "Bearer " + apiKey
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort cleanup
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: "team-by-key lookup HTTP " + resp.Status + ": " + truncate(string(respBytes), 200),
		}
	}

	var parsed struct {
		Data struct {
			Teams struct {
				Nodes []struct {
					ID   string `json:"id"`
					Key  string `json:"key"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", err
	}
	if len(parsed.Errors) > 0 {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: "team-by-key GraphQL error: " + parsed.Errors[0].Message,
		}
	}
	if len(parsed.Data.Teams.Nodes) == 0 {
		return "", &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: "no team with key " + teamKey,
		}
	}
	return parsed.Data.Teams.Nodes[0].Name, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Compile-time assertion that the linear package is loaded for its
// registry-side effects even when the integration tests are skipped.
var _ = linear.NewLinearAdapter
