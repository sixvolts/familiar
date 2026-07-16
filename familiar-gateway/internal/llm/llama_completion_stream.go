package llm

import "strings"

// completionStreamState owns the partial-token state machine that
// turns llama-server's /completion SSE chunks into clean reasoning /
// content / tool-call streams. Tag-configurable so the same machine
// works for Qwen (<think>), Cohere2 (<|START_THINKING|>), etc.
//
// States:
//
//	reasoning  — until we see thinkClose, everything goes to
//	             onReasoningChunk
//	content    — visible tokens forwarded to onContentChunk
//	             unless we're buffering through a tool call
//	in-tool    — buffering until toolClose lands
type completionStreamState struct {
	thinking    bool
	inReasoning bool
	buf         strings.Builder
	pending     string
	onContent   func(string)
	onReasoning func(string)
	inToolCall  bool
	toolCallBuf strings.Builder

	// Configurable tags per model family.
	thinkOpen  string
	thinkClose string
	toolOpen   string
	toolClose  string

	// When deferContent is true, content chunks are buffered instead
	// of emitted immediately. On flush(), the buffer is passed through
	// reasoningSplitter to separate untagged chain-of-thought from the
	// actual answer (needed for Command A Plus which reasons without
	// tags). The clean content is emitted; reasoning goes to onReasoning.
	deferContent      bool
	deferredContent   strings.Builder
	reasoningSplitter func(string) (string, string, bool)
}

func newCompletionStreamState(tags StreamTagConfig, enableThinking bool, onContent, onReasoning func(string)) *completionStreamState {
	return &completionStreamState{
		thinking: enableThinking,
		// Start in reasoning mode only if thinking is enabled AND the
		// formatter pre-fills the thinking-open tag in the prompt (Qwen).
		// Formatters like cohere2 emit thinking tags in the response
		// body, so they start in content mode and detect them on the fly.
		// The startInReasoning flag defaults to true for backward compat;
		// cohere2 overrides it via StreamTags().
		inReasoning: enableThinking && !tags.DetectThinkingInBody,
		onContent:   onContent,
		onReasoning: onReasoning,
		thinkOpen:   tags.ThinkOpen,
		thinkClose:  tags.ThinkClose,
		toolOpen:    tags.ToolOpen,
		toolClose:   tags.ToolClose,
	}
}

// SetDeferredContent enables content buffering with a post-hoc
// reasoning splitter. Used for models that reason without tags.
func (s *completionStreamState) SetDeferredContent(splitter func(string) (string, string, bool)) {
	s.deferContent = true
	s.reasoningSplitter = splitter
}

// emitContent either sends content to the callback or buffers it.
func (s *completionStreamState) emitContent(text string) {
	if text == "" {
		return
	}
	if s.deferContent {
		s.deferredContent.WriteString(text)
		return
	}
	if s.onContent != nil {
		s.onContent(text)
	}
}

func (s *completionStreamState) feed(text string) {
	if text == "" {
		return
	}
	s.buf.WriteString(text)
	s.pending += text
	for s.consume() {
	}
}

func (s *completionStreamState) consume() bool {
	if s.pending == "" {
		return false
	}

	if s.inToolCall {
		if idx := strings.Index(s.pending, s.toolClose); idx >= 0 {
			end := idx + len(s.toolClose)
			s.toolCallBuf.WriteString(s.pending[:end])
			s.pending = s.pending[end:]
			s.inToolCall = false
			return true
		}
		return false
	}

	if s.inReasoning {
		if idx := strings.Index(s.pending, s.thinkClose); idx >= 0 {
			if s.onReasoning != nil && idx > 0 {
				s.onReasoning(s.pending[:idx])
			}
			s.pending = s.pending[idx+len(s.thinkClose):]
			s.pending = strings.TrimLeft(s.pending, "\n")
			s.inReasoning = false
			return true
		}
		hold := tagPrefixHold(s.pending, s.thinkClose)
		if hold < len(s.pending) {
			if s.onReasoning != nil {
				s.onReasoning(s.pending[:len(s.pending)-hold])
			}
			s.pending = s.pending[len(s.pending)-hold:]
		}
		return false
	}

	// Content state — also detect thinking open tag for models
	// that emit it (Cohere2 emits <|START_THINKING|> in the
	// response body; Qwen's is in the prompt prefix).
	if s.thinkOpen != "" {
		if idx := strings.Index(s.pending, s.thinkOpen); idx >= 0 {
			if idx > 0 {
				s.emitContent(s.pending[:idx])
			}
			s.pending = s.pending[idx+len(s.thinkOpen):]
			s.inReasoning = true
			return true
		}
	}

	// Watch for tool-call opening.
	if idx := strings.Index(s.pending, s.toolOpen); idx >= 0 {
		if idx > 0 {
			s.emitContent(s.pending[:idx])
		}
		s.toolCallBuf.WriteString(s.pending[idx : idx+len(s.toolOpen)])
		s.pending = s.pending[idx+len(s.toolOpen):]
		s.inToolCall = true
		return true
	}

	// Hold back partial tag prefixes.
	minHold := len(s.pending)
	for _, tag := range []string{s.thinkOpen, s.toolOpen} {
		if tag == "" {
			continue
		}
		h := tagPrefixHold(s.pending, tag)
		if len(s.pending)-h < minHold {
			minHold = len(s.pending) - h
		}
	}
	if minHold > 0 {
		s.emitContent(s.pending[:minHold])
		s.pending = s.pending[minHold:]
	}
	return false
}

func (s *completionStreamState) flush() {
	if s.pending != "" {
		if s.inReasoning {
			if s.onReasoning != nil {
				s.onReasoning(s.pending)
			}
		} else if !s.inToolCall {
			s.emitContent(s.pending)
		}
		s.pending = ""
	}

	// If content was deferred, run the reasoning splitter now and
	// emit the clean content (and any extracted reasoning).
	if s.deferContent && s.deferredContent.Len() > 0 {
		raw := s.deferredContent.String()
		if s.reasoningSplitter != nil {
			if reasoning, content, ok := s.reasoningSplitter(raw); ok {
				if s.onReasoning != nil && reasoning != "" {
					s.onReasoning(reasoning)
				}
				if s.onContent != nil && content != "" {
					s.onContent(content)
				}
				return
			}
		}
		// No split found — emit everything as content.
		if s.onContent != nil {
			s.onContent(raw)
		}
	}
}

func (s *completionStreamState) full() string { return s.buf.String() }

func tagPrefixHold(text, tag string) int {
	maxLen := len(tag) - 1
	if maxLen > len(text) {
		maxLen = len(text)
	}
	for n := maxLen; n > 0; n-- {
		suffix := text[len(text)-n:]
		if strings.HasPrefix(tag, suffix) {
			return n
		}
	}
	return 0
}
