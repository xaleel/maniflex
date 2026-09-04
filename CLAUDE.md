When instructed to do a task or fix a bug from the readiness audit:

- do a quick check on validity of the description, don't assume it's true but don't waste too much time on it
- present the issue in one short clear sentence (what the issue means, why it's an issue, why it happened)
- present options to fix it, ups and downs of each approach, and mark the recommended one
- never alter established behavior or public API without permission, but don't do work arounds to avoid it, just use the ask user tool
- never use sub agents, always inline execution
- CHANGELOG.md entries must be short and relevant for those who want to upgrade version (e.g. don't include doc updates). As a general rule of thumb, each entry should be <500 characters
- never stage/commit anything to git, but you can use stash for teeth check
