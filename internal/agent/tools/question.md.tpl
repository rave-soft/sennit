Ask the user a structured question and wait for their response. Use this
when you need clarification, confirmation, or a choice before proceeding.

## How it works

Provide a `questions` array with at least one item. A single item renders
as a plain question; multiple items render as a tabbed form with a
confirmation screen at the end.

Every question MUST include:
- `type` — `yes_no`, `single_choice`, `multi_choice`, or `free_text`
- `question` — a short, direct question (one line), under {{ .MaxQuestionLength }} chars
- `description` — markdown context shown below the question with details,
  tradeoffs, or examples. **Always required.** Omitting it causes an error.

## Hard limits (enforced; violations error and waste a round trip)

- Max {{ .MaxChoices }} choices per question; group or prioritize if you have more.
- `single_choice`/`multi_choice` require `choices` (a single_choice without
  choices is an error), and every choice needs a unique `id` — a repeated
  `id` within the same question is an error.
- `description` required on every question, under {{ .MaxDescriptionLength }} chars.
  Choice `label` under {{ .MaxChoiceLabelLength }} chars, choice `description`
  under {{ .MaxChoiceDescriptionLength }} chars each.
- Max {{ .MaxQuestions }} questions per batch — split into multiple batches and tell the
  user there will be follow-ups if you need more.

## Question types

- `yes_no` — confirmation only, for a proposition the user affirms or
  rejects ("Proceed with deletion?", "Enable caching?"). Never use it for
  A-vs-B choices or preference questions where both answers are valid
  options rather than accept/reject — use `single_choice` instead, even
  for exactly two options (e.g. "TypeScript or Go?").
- `single_choice` — pick one from `choices`, for any selection between
  named alternatives, including binary ones. At least 2 choices required.
- `multi_choice` — pick one or more from `choices`.
- `free_text` — open-ended narrative answer (e.g. "What keeps you up at
  night?"). No choices needed; don't use yes_no for open-ended questions.

Single/multi choice questions automatically include a free-text fill-in
option so the user can type a custom answer — do not add an "Other" or
"Custom" choice manually.

## Confirmation screen (batches only)

When asking multiple questions, a confirmation tab is always shown after
all questions are answered: the user sees a summary of their answers and
must confirm before submitting, or go back to editing if they decline.

Both fields below are optional — a generic title/description fills in if
omitted — but a specific one reads much better:
- `confirm_title`: a short question, e.g. "Ready to go?"
- `confirm_description`: summarize what will happen based on the expected
  answers, written as if you already know what they'll pick.

## Multiple questions

Each item can include an optional `label` (3 words max) used as the tab
header; if omitted, the first 3 words of `question` are used.

Example — multiple questions with confirmation:
```json
{
  "questions": [
    {"label": "Database", "type": "single_choice", "question": "Which database?", "description": "PostgreSQL for relational data, MongoDB for documents.", "choices": [{"id": "pg", "label": "PostgreSQL"}, {"id": "mongo", "label": "MongoDB"}]},
    {"label": "Caching", "type": "yes_no", "question": "Enable caching?", "description": "Reduces latency for repeated queries but adds invalidation complexity."}
  ],
  "confirm_title": "Ready to configure?",
  "confirm_description": "We'll set up PostgreSQL with query caching enabled."
}
```

## When to use

Confirm destructive or ambiguous actions; the request has multiple valid
interpretations; need the user to pick from options; gather multiple
related answers at once.

## When NOT to use

Questions answerable by reading code or docs; information obtainable via
other tools; asking permission (use the permission system instead).
