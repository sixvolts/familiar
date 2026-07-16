package admin

// Owner-side shard chat (SKILL-PACKAGES-SPEC Phase 1): a conversation
// whose model is "shard:<id>" runs through the shard's envelope
// instead of the trusted path. This file owns the policy — who may
// bind, and what a binding resolves to — in one place, shared by the
// conversation create/patch handlers and the /api/chat resolver that
// main.go hands the native adapter.

import (
	"context"
	"fmt"
	"strings"

	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/shards"
)

// shardModelPrefix marks a shard-bound conversation in the
// conversations.model column ("shard:<shard-id>").
const shardModelPrefix = "shard:"

// validateShardChatBinding checks a conversation model value at
// write time. Empty return = fine (including the non-shard case);
// non-empty is a user-facing refusal. The same conditions are
// re-checked per message by ResolveShardChat — this gate just keeps
// rows from being born unusable.
func (h *Handler) validateShardChatBinding(ctx context.Context, model, userID string) string {
	if !strings.HasPrefix(model, shardModelPrefix) {
		return ""
	}
	_, refusal, err := h.resolveShardForChat(ctx, strings.TrimPrefix(model, shardModelPrefix), userID)
	if err != nil {
		return "could not verify shard binding: " + err.Error()
	}
	return refusal
}

// ShardChatTargetInfo is the resolved envelope for one shard-bound
// conversation. The native adapter adapts this into its own type —
// admin must not import the adapter.
type ShardChatTargetInfo struct {
	ShardID   string
	Ephemeral bool
	Overrides *pipeline.ShardOverrides
}

// ResolveShardChat maps an owned conversation to its shard target.
// (nil, "", nil) = trusted-path conversation. A non-empty refusal is
// a policy rejection ("shard is disabled", ...); err is internal.
// Conversation ownership is enforced here too (the store Get is
// owner-scoped) even though the adapter has already checked it —
// re-deriving is cheaper than trusting call order.
func (h *Handler) ResolveShardChat(ctx context.Context, conversationID, userID string) (*ShardChatTargetInfo, string, error) {
	if h.conversations == nil {
		return nil, "", nil
	}
	c, err := h.conversations.Get(ctx, conversationID, userID)
	if err != nil {
		return nil, "", fmt.Errorf("load conversation: %w", err)
	}
	if !strings.HasPrefix(c.Model, shardModelPrefix) {
		return nil, "", nil
	}
	sh, refusal, err := h.resolveShardForChat(ctx, strings.TrimPrefix(c.Model, shardModelPrefix), userID)
	if err != nil || refusal != "" {
		return nil, refusal, err
	}
	return &ShardChatTargetInfo{
		ShardID:   sh.ID,
		Ephemeral: sh.Persistence == shards.PersistenceEphemeral,
		Overrides: pipeline.OverridesForShard(sh),
	}, "", nil
}

// resolveShardForChat loads the shard and applies the chat-binding
// policy: must exist, be the caller's, be enabled, and have
// chat_enabled. Not-found and not-owned collapse to one message so
// shard ids can't be probed through conversation bindings.
func (h *Handler) resolveShardForChat(ctx context.Context, shardID, userID string) (shard *shards.Shard, refusal string, err error) {
	if h.shards == nil {
		return nil, "shard chat is not configured on this gateway", nil
	}
	sh, err := h.shards.GetShard(ctx, shardID)
	if err != nil || sh.OwnerID != userID {
		return nil, "shard not found or not owned by you", nil
	}
	if !sh.Active() {
		return nil, "shard " + sh.ID + " is disabled", nil
	}
	if !sh.ChatEnabled {
		return nil, "chat is disabled for shard " + sh.ID, nil
	}
	return sh, "", nil
}
