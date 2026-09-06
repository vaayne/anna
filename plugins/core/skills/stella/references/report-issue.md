# Reporting a GitHub issue

When the user reports that you (stella) hit an error or behaved incorrectly and asks to file it — e.g. "报告这个 issue", "帮我建个 issue", "report this bug", "create an issue for this" — help them open a GitHub issue against the stella repo (`CherryHQ/stella`).

Never create an issue without explicit confirmation, and never run the flow without checking GitHub is linked first.

## Workflow

1. **Confirm intent and target repo.** Default repo is `CherryHQ/stella`. If the user means a different repo, ask before proceeding.
2. **Draft the issue.** Summarize the problem from the conversation and recent errors. Ask clarifying questions only when required context is missing. Use the standard structure below.
3. **Get explicit confirmation.** Show the user the drafted title and body. Do not create the issue until they approve.
4. **Check GitHub link status:** use `oauth_list`. If GitHub is not connected, run the OAuth flow (next section) before continuing. Do not ask the user to authenticate `gh` manually.
5. **Create the issue** once approved and authenticated. Use a heredoc for the body — a literal `\n` inside a double-quoted `--body` is written verbatim, not as a newline. Add a `bug` label (or `documentation`/`enhancement` if it fits better):
   ```bash
   gh issue create --repo CherryHQ/stella --label bug --label status:needs-triage \
     --title "<title>" \
     --body "$(cat <<'EOF'
   ## What
   ...
   EOF
   )"
   ```
6. **Return the created issue URL** to the user.
7. **Offer to star the repo.** Check whether it is already starred (`gh api /user/starred/CherryHQ/stella` returns 204 if starred, 404 if not). If not starred, offer to add a star — `gh api -X PUT /user/starred/CherryHQ/stella` — only with the user's consent.

## Issue structure

Follow the project's standard four-section format (the What/Why/How/Refs convention from the repo's issue template). A user-reported bug remains GitHub-only with `status:needs-triage` until a maintainer accepts and schedules it. Do not create a Feishu task or assign a release milestone during this reporting flow.

```markdown
## What

<one-line summary of the problem>

## Why

<impact — what the user couldn't do, or what broke>

## How

**Expected:** <what should have happened>
**Actual:** <what actually happened>
**Steps/context:** <how to reproduce or what triggered it>
**Logs/errors:** <relevant error output, redacted>

## Refs

<session/environment context if useful and safe; related issues>
```

## Linking GitHub via OAuth

If GitHub is not connected, initiate the device flow yourself with the `oauth_*` tools — do not tell the user to run CLI commands:

1. Call `oauth_connect` with `provider=github`.
2. Give the user the returned `verification_uri` and `user_code`.
3. Ask them to open the URL, enter the code, and authorize.
4. After they report back, call `oauth_flow_status` with the returned `flow_id`.
5. Once `connected` is `true`, `gh` works directly in `bash` — proceed to create the issue.

## Privacy and safety

- Never include secrets, tokens, credentials, API keys, or private user data in the issue body.
- Redact sensitive values from any logs you attach (replace with `<redacted>`).
- Include environment/session context only when it helps and is safe to publish — issues are public.
