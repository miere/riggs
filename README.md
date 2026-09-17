# Riggs

Riggs runs an AI agent on your machine and connects it to a chat gateway over
[RAX](https://github.com/miere/rax-protocol). It supports Claude Code and ACP agents.

## Crates

- `riggs-node`: serves RAX sessions onto one agent backend. It handles requests, turns, the tool
  gate, prompts, sign-ins, credential reports and the durable session store.
