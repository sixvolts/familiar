package sidecar

// ConversationContext is the recent-turns context the chat entry points
// (pipeline.Handle, HandleStream) accept. Nothing reads it any more: the
// classifier takes its history from the session.
type ConversationContext struct {
	PreviousTurns []ContextTurn
}

// ContextTurn is a single turn in the conversation context.
type ContextTurn struct {
	Role    string // "user" or "assistant"
	Content string
}
