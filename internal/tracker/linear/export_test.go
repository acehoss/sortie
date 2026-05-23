package linear

// NewAdapterForTest constructs a LinearAdapter against an arbitrary
// Client implementation. Exposed only during testing so that the
// linearmock subpackage can be used as the seam.
func NewAdapterForTest(client Client, teamKey string, activeStates []string) *LinearAdapter {
	return newAdapterWithClient(client, teamKey, activeStates)
}
