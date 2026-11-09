package tools

import "context"

// Question is one question the clarify tool puts to the user, with optional
// answers to pick from.
type Question struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
}

// Asker is implemented by approvers that can also put questions to the
// user, such as the terminal UI. The clarify tool asks through the turn's
// approver when it is one, before looking for a chat. Ask returns "" when
// the user gave no answer.
type Asker interface {
	Ask(ctx context.Context, questions []Question) (string, error)
}
