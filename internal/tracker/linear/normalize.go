package linear

import (
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// normalizeIssue converts a Linear wire-type Issue into the normalized
// [domain.Issue]. Comments are not populated by this function; callers
// that need them attach the result of a separate comment fetch.
func normalizeIssue(src *Issue) domain.Issue {
	labels := make([]string, 0, len(src.Labels))
	for _, lbl := range src.Labels {
		labels = append(labels, strings.ToLower(lbl.Name))
	}

	blocked := make([]domain.BlockerRef, 0, len(src.InverseRelations))
	for _, rel := range src.InverseRelations {
		if !strings.EqualFold(strings.TrimSpace(rel.Type), "blocks") {
			continue
		}
		blocked = append(blocked, domain.BlockerRef{
			ID:         rel.Issue.ID,
			Identifier: rel.Issue.Identifier,
			State:      rel.Issue.State,
		})
	}

	var parent *domain.ParentRef
	if src.Parent != nil {
		parent = &domain.ParentRef{
			ID:         src.Parent.ID,
			Identifier: src.Parent.Identifier,
		}
	}

	return domain.Issue{
		ID:          src.ID,
		Identifier:  src.Identifier,
		Title:       src.Title,
		Description: src.Description,
		Priority:    normalizePriority(src.Priority),
		State:       src.State.Name,
		BranchName:  src.BranchName,
		URL:         src.URL,
		Labels:      labels,
		Assignee:    normalizeAssignee(src.Assignee),
		Parent:      parent,
		BlockedBy:   blocked,
		CreatedAt:   src.CreatedAt,
		UpdatedAt:   src.UpdatedAt,
	}
}

// normalizeComment converts a Linear wire-type Comment into the
// normalized [domain.Comment].
func normalizeComment(src Comment) domain.Comment {
	return domain.Comment{
		ID:        src.ID,
		Author:    normalizeAssignee(src.User),
		Body:      src.Body,
		CreatedAt: src.CreatedAt,
	}
}

// normalizePriority maps Linear's Float priority (0–4) to a nullable
// integer. Linear convention: 0 = No priority, 1 = Urgent, 2 = High,
// 3 = Medium, 4 = Low. The domain's Priority field is *int where
// lower-is-higher; "No priority" becomes nil so it sorts to the
// orchestrator default position.
func normalizePriority(p float64) *int {
	if p < 1 || p > 4 {
		return nil
	}
	i := int(p)
	return &i
}

// normalizeAssignee picks the first non-empty of DisplayName, Name,
// Email and returns "" when the user is nil or all fields are empty.
// Used for both Issue.Assignee and Comment.Author.
func normalizeAssignee(u *User) string {
	if u == nil {
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}
