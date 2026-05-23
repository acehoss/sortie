package linear

// GraphQL query and mutation strings used by the real Client
// implementation. Each constant matches the wire-type field selections
// in types.go — adding a field to a wire type requires adding it here
// (and vice versa).

const (
	queryIssues = `
query SortieLinearIssues($filter: IssueFilter!, $first: Int!, $relationFirst: Int!, $after: String) {
  issues(filter: $filter, first: $first, after: $after) {
    nodes {
      id
      identifier
      title
      description
      priority
      branchName
      url
      createdAt
      updatedAt
      state { id name type }
      assignee { displayName name email }
      parent { id identifier }
      team { id }
      labels(first: 50) { nodes { id name } }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue { id identifier state { name } }
        }
      }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

	queryIssueByKey = `
query SortieLinearIssue($id: String!, $relationFirst: Int!) {
  issue(id: $id) {
    id
    identifier
    title
    description
    priority
    branchName
    url
    createdAt
    updatedAt
    state { id name type }
    assignee { displayName name email }
    parent { id identifier }
    team { id }
    labels(first: 50) { nodes { id name } }
    inverseRelations(first: $relationFirst) {
      nodes {
        type
        issue { id identifier state { name } }
      }
    }
  }
}`

	queryIssueComments = `
query SortieLinearIssueComments($id: String!, $first: Int!, $after: String) {
  issue(id: $id) {
    id
    comments(first: $first, after: $after, orderBy: createdAt) {
      nodes {
        id
        body
        createdAt
        user { displayName name email }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

	queryStateIDByName = `
query SortieLinearResolveStateID($issueId: String!, $stateName: String!) {
  issue(id: $issueId) {
    id
    team {
      states(filter: { name: { eqIgnoreCase: $stateName } }, first: 1) {
        nodes { id }
      }
    }
  }
}`

	queryIssueLabels = `
query SortieLinearIssueLabels($id: String!, $first: Int!, $after: String) {
  issue(id: $id) {
    id
    team { id }
    labels(first: $first, after: $after) {
      nodes { id name }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

	queryTeamLabels = `
query SortieLinearTeamLabels($teamId: String!, $first: Int!) {
  team(id: $teamId) {
    id
    labels(first: $first) { nodes { id name } }
  }
}`

	mutationIssueUpdateState = `
mutation SortieLinearIssueUpdateState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
    issue { id state { id name } }
  }
}`

	mutationIssueUpdateLabels = `
mutation SortieLinearIssueUpdateLabels($id: String!, $labelIds: [String!]!) {
  issueUpdate(id: $id, input: { labelIds: $labelIds }) {
    success
  }
}`

	mutationCommentCreate = `
mutation SortieLinearCommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id }
  }
}`

	mutationIssueLabelCreate = `
mutation SortieLinearIssueLabelCreate($teamId: String!, $name: String!) {
  issueLabelCreate(input: { teamId: $teamId, name: $name }) {
    success
    issueLabel { id name }
  }
}`
)
