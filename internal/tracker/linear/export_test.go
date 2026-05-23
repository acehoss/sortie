package linear

// NewAdapterForTest constructs a LinearAdapter against an arbitrary
// Client implementation. Exposed only during testing so that the
// linearmock subpackage can be used as the seam. Pass "" for
// assigneeID to disable assignee filtering.
func NewAdapterForTest(client Client, teamKey string, activeStates []string, assigneeID string) *LinearAdapter {
	return newAdapterWithClient(client, teamKey, activeStates, assigneeID)
}

// ResolveAssigneeForTest exposes the package-internal resolveAssignee
// helper so external tests can verify the "me" → viewer lookup path
// and the UUID pass-through path without going through NewLinearAdapter
// (which hardwires the real HTTP client).
func ResolveAssigneeForTest(client Client, raw any) (string, error) {
	return resolveAssignee(client, raw)
}
