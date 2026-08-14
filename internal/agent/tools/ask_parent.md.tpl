Send a message to the session that created this delegation (its "parent"),
so the parent's agent sees it on its next turn — or wakes it if it is idle.

This call is non-blocking: it returns immediately and does not wait for a
reply. Keep working (or finish the delegation) after asking; do not stop
and wait here. If the parent answers, the answer arrives later as an
ordinary follow-up prompt to this same session, not as a return value of
this call.

Use it sparingly. Asking costs the user's attention, so reserve it for a
genuine fork in the work where guessing wrong is costly — for example,
"which of these two conflicting APIs did you mean" or "the merge conflicts
with the base branch, should I rebase or should you resolve it by hand."
Routine judgment calls the delegation can make on its own do not belong
here.

If this session has no registered parent — it was not created as part of
a delegation, or it was created without one — the call fails cleanly.
Proceed on your own judgment instead of expecting an answer.

Parameters:
- `message` (required): the question or update to send to the parent.
