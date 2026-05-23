package linear

import (
	"context"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Trackers.RegisterWithMeta("linear", NewLinearAdapter, registry.TrackerMeta{
		RequiresProject: true,
		RequiresAPIKey:  true,
	})
}

// NewLinearAdapter constructs a [LinearAdapter] from adapter
// configuration. Required config keys: "api_key" (Linear personal API
// key, sent verbatim in the Authorization header), "project" (Linear
// team key, e.g. "ENG"). Optional: "endpoint" (defaults to
// https://api.linear.app/graphql), "use_bearer" (set to true when
// api_key is an OAuth access token), "user_agent", "active_states"
// (defaults to ["Backlog", "Todo"]), "assignee" (Linear user UUID, or
// "me" to resolve via the viewer query — restricts candidate issues
// to those assigned to the given user).
func NewLinearAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	teamKey, activeStates, err := extractAdapterConfig(config)
	if err != nil {
		return nil, err
	}

	apiKey, _ := config["api_key"].(string)
	if apiKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "missing required config key: api_key",
		}
	}

	endpoint, _ := config["endpoint"].(string)
	endpoint = strings.TrimRight(endpoint, "/")

	useBearer, _ := config["use_bearer"].(bool)
	userAgent, _ := config["user_agent"].(string)

	client, err := NewClient(ClientOptions{
		Endpoint:  endpoint,
		APIKey:    apiKey,
		UseBearer: useBearer,
		UserAgent: userAgent,
	})
	if err != nil {
		return nil, err
	}

	assigneeID, err := resolveAssignee(client, config["assignee"])
	if err != nil {
		return nil, err
	}

	return newAdapterWithClient(client, teamKey, activeStates, assigneeID), nil
}

// resolveAssignee converts the workflow's assignee config value into a
// concrete Linear user UUID. Returns empty when no filter is requested.
// The literal string "me" triggers a viewer query against Linear; any
// other non-empty string is passed through unchanged on the assumption
// that it is already a UUID.
func resolveAssignee(client Client, raw any) (string, error) {
	s, _ := raw.(string)
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if s != "me" {
		return s, nil
	}
	id, err := client.QueryViewer(context.Background())
	if err != nil {
		return "", err
	}
	return id, nil
}
