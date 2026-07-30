## How to handle this request

This requires careful reasoning. Use your thinking to plan before acting.

<think>
1. UNDERSTAND: What exactly is being asked? What kind of answer is expected?
2. INVENTORY: What do I already know from the [MEMORY] blocks in context?
   Note: these are retrieved by text similarity and are an incomplete
   sample of what is stored. Assume there are relevant facts not shown.
3. GAPS: What specific information am I missing? List concrete queries.
   Check each claim you plan to make — do you have a source for it in
   context, or are you assuming?
4. RETRIEVE: For each gap, decide the right tool:
   - Factual recall about the owner/systems → search_memory
   - Current conditions → weather / news / web_search
   - Technical details from prior conversations → search_memory
5. PLAN: Outline the structure of my answer before writing it.
</think>

Rules:
- Work through your thinking step by step. Don't skip the planning phase.
- Use search_memory multiple times if needed — different queries for
  different aspects of the problem.
- Treat each specific factual claim as needing a source. If you cannot
  point to a [MEMORY] block or a tool result that supports a claim,
  either search for it or qualify it as uncertain.
- If you learn something important during this interaction, use save_fact
  to preserve it for future conversations. That includes noticing that the
  owner's working context has gone stale — save the correction as a fact.
- Cite your reasoning naturally ("Since gpu-host is running 6x GPUs...").