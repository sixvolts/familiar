package main

import (
	"context"

	"github.com/familiar/gateway/internal/admin"
)

// slackConvStore projects *admin.ConversationStore into the narrow
// slackadapter.Conversations interface (primitive args, no admin types)
// so the Slack adapter can persist + hydrate durable conversations
// without importing the admin package. Mirrors the modelCatalog
// adapter pattern. SLACK-CONTEXT.
type slackConvStore struct{ cs *admin.ConversationStore }

func (s slackConvStore) EnsureExternalConversation(ctx context.Context, userID, externalKey, title string) (string, error) {
	c, err := s.cs.EnsureExternalConversation(ctx, userID, externalKey, title)
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func (s slackConvStore) AppendMessage(ctx context.Context, convID, role, content, model string) error {
	_, err := s.cs.AppendMessage(ctx, &admin.Message{
		ConversationID: convID,
		Role:           role,
		Content:        content,
		Model:          model,
	})
	return err
}
