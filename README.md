# Riggs

Riggs runs an AI agent on your machine and connects it to a chat gateway over
[RAX](https://github.com/miere/rax-protocol). It supports Claude Code and ACP agents.

## Crates

- `riggs-node`: serves RAX sessions onto one agent backend. It handles requests, turns, the tool
  gate, prompts, sign-ins, credential reports and the durable session store.
- `riggs-claude-code`: runs Claude Code as that backend. Each session gets one `claude` process,
  and every tool call waits at a PreToolUse hook until the gateway rules on it.
- `riggs-process`: starts each agent in its own process group and kills it with everything it
  spawned, including processes that left the group.
- `riggs-e2e`: end-to-end tests. They run the node against the RAX gateway simulator, with a
  scripted `fake-claude` built from recorded Claude Code transcripts.
