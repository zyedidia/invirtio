Keep code and comments minimal. Comments should only describe the current code,
never changes between iterations or steps in a plan.

Do not use semicolons or unicode symbols in comments.

Comments should be sentences and end with a period.

Place comments on a separate line (before) from code for non-assembly files.

Comments describe the current state of the code in plain, professional
language. Do not use informal terms, and do not refer to history, future plans,
or what the code does not do yet. Prefer removing a comment to keeping one that
restates what the code makes clear. Use natural prose. Avoid lists of examples
if they are unnecessary (especially multiple examples), or cliche grammatical
structures.

For code review we use review loops: write a REVIEW-<title>.md file in the
repository root that describes the patch to a code reviewer. Then spawn a
subagent to perform code review, and apply the fixes it suggests after
verifying them. Then continue spawning review subagents until no more issues
are found.

Commits for this project are always title-only with no body.
