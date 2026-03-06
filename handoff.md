# Session Persistence Handoff

## What was done
Implemented Pi-compatible session persistence in `agent/store/`. Sessions are saved as JSONL files matching Pi's `SessionManager` format (v3), enabling future session sharing between anna and Pi.

## Files changed
- `agent/store/store.go` — `Store` interface + `FileStore` (Pi JSONL format)
- `agent/store/store_test.go` — 11 unit tests (round-trip, format, parent chain)
- `agent/store/real_pi_test.go` — loads real Pi session files from `~/.pi/agent/sessions`
- `agent/pool.go` — integrated store (persist on chat, restore on load, delete on reset)
- `agent/runner/runner.go` — added `AssistantMessageToRPCEvent()`
- `agent/runner/go/runner.go` — fixed `convertHistory()` to handle `Summary` field
- `main.go` — wires `FileStore` into Pool
- `go.mod` / `go.sum` — added `github.com/google/uuid`

## Known issues to fix

### 1. Assistant messages with mixed text+toolCall lose text on load
When Pi stores an assistant message like `{"content":[{"type":"text","text":"Let me check..."},{"type":"toolCall",...}]}`, our `entryToRPCEvent()` returns only the tool call, dropping the text. The flat `RPCEvent` model can't represent both in one event.

**Fix approach**: Emit multiple RPCEvents from a single entry — one `message_update` for text, one `tool_call` per tool call. Change `entryToRPCEvent()` to return `[]RPCEvent`.

### 2. Tool results without matching tool calls after compaction
Pi's compaction summarizes old history but keeps some tool results. These orphaned results load fine but have no matching call in our history. The go runner's `convertHistory()` may not handle this gracefully.

**Fix approach**: Audit `convertHistory()` to handle orphaned tool results (skip or attach to a synthetic call).

### 3. No session file naming convention for timestamp-based sorting
Anna uses `{sessionID}.jsonl` as filename. Pi uses `{timestamp}_{uuid}.jsonl`. This means anna sessions won't sort chronologically in Pi's session picker.

**Fix approach**: Use Pi's naming convention `{ISO-timestamp}_{sessionID}.jsonl` in `FileStore.path()`.

### 4. No compaction/branch_summary support on load
Pi sessions may contain `compaction` and `branch_summary` entries. Our loader skips them, which means loaded Pi sessions lose compacted context.

**Fix approach**: Parse `compaction` entries and inject the summary as a synthetic user/assistant exchange, similar to how Pi's `buildSessionContext()` handles it.

### 5. User content format mismatch
Anna stores user content as plain string (`"content":"hello"`). Pi stores it as content block array (`"content":[{"type":"text","text":"hello"}]`). Both load fine, but Pi's UI may render anna's string format differently.

**Fix approach**: Always write user content as content block array in `rpcEventToEntry()`.
