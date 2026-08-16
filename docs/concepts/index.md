# Concepts

Two things about Sennit are worth understanding properly, because guessing
wrong about either leads to a surprise rather than a bug report.

## [Steering, tasks and threads](delegation.md)

Three ways work happens alongside the current turn, with very different costs.
The one that catches people out: sending a message while the agent is working
does **not** interrupt it — the message is folded into the running turn. To
actually stop, press <kbd>Esc</kbd> twice.

## [Sessions and data storage](sessions.md)

Where conversations, messages and file history are kept — one shared SQLite
database for every project, not one per project — and how to inspect, resume
and prune them.
