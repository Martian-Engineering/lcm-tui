# lcm-tui transplant — Context Transplant Spec

## Problem

When a session resets (context overflow, manual reset, etc.), OpenClaw starts a new conversation. The new conversation has no LCM history — it's a blank slate. All the accumulated summaries from the previous conversation are orphaned. The agent loses long-term context.

## Goal

Copy the active context summaries from a source conversation into a target conversation, so the target inherits the source's compacted knowledge. The target conversation then continues building on that foundation as if the session had never reset.

## Approach: Deep Copy of Active Context Summaries

Copy the summary-type context items from the source conversation into the target, creating new first-class summary rows owned by the target. No cross-conversation references — transplanted summaries are indistinguishable from native ones.

### Why deep copy

- Transplanted summaries become first-class citizens in the target conversation
- No cross-conversation FK weirdness — integrity checker works as-is
- Future compaction naturally condenses them (they're just regular summaries)
- No special-case logic needed anywhere in the system

### What we transplant

Only **summary-type context items** from the source. Not messages — those are conversation-specific.

For each source summary, we copy:
- The summary row itself (new `summary_id`, target `conversation_id`, same content/kind/depth/token_count/file_ids)
- Its `summary_parents` edges (remapped to new IDs)
- Its `summary_messages` edges (kept pointing to original message IDs — messages are shared/immutable)

### Schema recap

```sql
-- context_items: the ordered context window
(conversation_id, ordinal, item_type, message_id, summary_id, created_at)

-- summaries: the actual summary content  
(summary_id, conversation_id, kind, content, token_count, created_at, file_ids, depth)

-- summary_parents: DAG edges (child → parent)
(summary_id, parent_summary_id, ordinal)

-- summary_messages: leaf → message mappings
(summary_id, message_id, ordinal)
```

### Algorithm

```
transplant(source_conv, target_conv):
  1. Read source's summary-type context items (ordered by ordinal)
  2. Collect the full set of summaries to copy:
     - The context summaries themselves
     - Recursively walk summary_parents to get the full DAG beneath them
     - Deduplicate (a parent may be shared by multiple children)
  3. Topological sort: copy parents before children (leaves first, then d1, d2, ...)
  4. For each summary in topo order:
     a. Generate new summary_id (same format: sum_<16 hex chars>)
     b. INSERT into summaries with conversation_id = target
     c. Copy summary_messages rows (same message_ids, new summary_id)
     d. Copy summary_parents rows (remapped: both summary_id and parent_summary_id use new IDs)
     e. Record old_id → new_id mapping
  5. Shift target's existing context_items ordinals up by len(source_context_summaries)
  6. Insert new context_items at ordinals 0..N-1, pointing to new summary_ids
     (only for the summaries that were in source's context — not the full DAG)
  7. Report what was transplanted
```

### Why copy the full DAG (not just context summaries)

The context items reference the "top" of the DAG (the highest-depth condensed summaries + recent leaves). But `lcm_expand_query` walks `summary_parents` to expand deeper. If we only copy the top-level summaries, expansion would hit dead ends or cross-conversation references. Copying the full DAG makes expansion work natively.

### Detailed steps

**Step 1: Read source context summaries**
```sql
SELECT ci.ordinal, ci.summary_id, s.depth, s.kind, s.token_count, s.content, s.file_ids
FROM context_items ci
JOIN summaries s ON ci.summary_id = s.summary_id
WHERE ci.conversation_id = :source AND ci.item_type = 'summary'
ORDER BY ci.ordinal;
```

**Step 2: Walk the DAG recursively**
Starting from the context summary IDs, follow `summary_parents` down:
```sql
-- Recursive CTE to find all ancestors
WITH RECURSIVE ancestors(sid) AS (
  VALUES (:context_summary_id_1), (:context_summary_id_2), ...
  UNION
  SELECT sp.parent_summary_id
  FROM summary_parents sp
  JOIN ancestors a ON sp.summary_id = a.sid
)
SELECT DISTINCT s.*
FROM ancestors a
JOIN summaries s ON s.summary_id = a.sid;
```

**Step 3: Topological sort by depth**
Sort ascending by depth (d0 first, then d1, d2). Within same depth, preserve creation order.

**Step 4: Copy summaries**
For each summary in topo order:
```sql
-- New summary
INSERT INTO summaries (summary_id, conversation_id, kind, content, token_count, created_at, file_ids, depth)
VALUES (:new_id, :target_conv, :kind, :content, :token_count, :created_at, :file_ids, :depth);

-- Copy message mappings (leaves only)
INSERT INTO summary_messages (summary_id, message_id, ordinal)
SELECT :new_id, message_id, ordinal
FROM summary_messages WHERE summary_id = :old_id;

-- Copy parent edges (remapped)
INSERT INTO summary_parents (summary_id, parent_summary_id, ordinal)
SELECT :new_id, :remapped_parent_id, ordinal
FROM summary_parents WHERE summary_id = :old_id;
```

**Step 5-6: Update context_items**
```sql
-- Shift existing ordinals
UPDATE context_items
SET ordinal = ordinal + :num_context_summaries
WHERE conversation_id = :target;

-- Insert transplanted context items
INSERT INTO context_items (conversation_id, ordinal, item_type, summary_id)
VALUES (:target, :ordinal, 'summary', :new_summary_id);
```

All in a single transaction.

### CLI interface

```
lcm-tui transplant <source_conversation_id> <target_conversation_id> [--dry-run] [--apply]
```

Default is dry-run (show what would happen). Must pass `--apply` to execute.

**Dry-run output:**
```
Transplant: conversation 553 → conversation 642

Source context summaries (14):
  sum_c83e88d80bc2025e  condensed  d2  1790t  "Status update with completed overnight..."
  sum_65089aa5f17ac263  condensed  d1  1700t  "Josh asked about rebasing main into..."
  sum_2a983d00ad0496df  leaf       d0   640t  "..."
  ...

Full DAG to copy: 80 summaries (14 context + 66 ancestors)
  d0: 58 leaves
  d1: 18 condensed
  d2: 4 condensed

Target current context (145 items):
  5 summaries + 140 messages

After transplant:
  14 new context items prepended
  80 summaries copied (new IDs, owned by conversation 642)
  Estimated token overhead in context: ~7,496 tokens

Run with --apply to execute.
```

### What we copy

- **Summary rows** — new IDs, target conversation_id, same content
- **summary_parents edges** — remapped to new IDs
- **summary_messages edges** — same message_ids (messages are shared), new summary_id

### What we do NOT copy

- **Messages** — they stay in the source. summary_messages in the target will reference source message_ids. This is fine: messages are immutable and the FK allows cross-conversation references.
- **Source context_items** — we create new ones for the target.

### Edge cases

1. **Target already has transplanted summaries** — detect by checking if any source summary content already exists in target (content hash comparison), warn and abort
2. **Source has no summary context items** — nothing to transplant, warn and exit
3. **Source summaries were corrupted/repaired** — copies the current (repaired) content
4. **summary_messages pointing to source messages** — acceptable cross-conversation reference. Messages are immutable rows. lcm_expand walks summaries → summary_messages → messages, and message_id is globally unique.
5. **Future compaction in target** — works naturally. LCM sees the transplanted summaries as regular context items, condenses them into new d1/d2 summaries owned by target. The transplanted DAG becomes the foundation layer.

### Integrity

No special handling needed. All summaries are owned by the target conversation. The integrity checker works as-is. The only cross-conversation reference is `summary_messages` pointing to messages in the source conversation, which is structurally valid (message_id is a global FK).

### Token budget

The 14 source summaries total ~7,496 tokens. With the new 200k context window, this is ~3.7% — negligible overhead for full historical context.
