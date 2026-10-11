# Usage

Open Usage from the home screen or a computer's controls. Select a computer to see its Codex and Claude subscription limits, reset times and local activity.

## Subscription Limits

Limits belong to the account signed in on that computer. Two computers using the same account share its quota. The relay reads the provider's current usage endpoint and refreshes the view while it is open. The reset countdown updates between readings.

Claude plan multipliers come from its saved subscription tier. When Codex reports only a generic plan name, a host-local `usage-plans.json` annotation can supply a known tier label. The annotation is bound to a hash of the account ID and the reported base plan, so it does not apply after switching accounts. This is a display label, and does not change quota calculations.

A missing or expired sign-in produces an explanation rather than a guessed percentage. Local history remains available. Open Claude Code or Codex on that computer to refresh its sign-in, then refresh Usage.

## Local Tokens and Costs

Today uses the computer's local calendar day. The 30-day period includes today and the preceding 29 calendar days. Archived sessions are included when their recorded activity falls in that period.

The scanner reads local request records and deduplicates repeated messages and copied session history. It handles cache reads, cache writes, one-hour Claude cache writes and published long-context rates. Repeated usage snapshots do not count as another request. Input totals include cached prompt tokens; output totals include recorded reasoning usage.

Dollar amounts estimate the API value of the recorded activity. They are separate from subscription charges and use known model prices checked on October 11, 2026. Models without a known rate retain their token counts and are marked as unpriced. A plus sign indicates that the cost total excludes unpriced activity. Provider rates can change; estimates use the table shipped with fastcontrol.

Pricing sources: [OpenAI](https://developers.openai.com/api/docs/pricing) and [Anthropic](https://platform.claude.com/docs/en/about-claude/pricing).

## Data and Access

`GET /usage` requires the relay's pairing token. It returns aggregates, plan labels and limit windows. Provider credentials remain on the computer. The relay reads local sign-in data and sends it only to the corresponding provider's usage endpoint.

Refresh requests share one snapshot worker. Its file index reuses unchanged logs, removes deleted files and publishes the finished snapshot atomically. Network reads have deadlines. Large unrelated tool-output lines are discarded during scanning; a skipped billable entry is reported as incomplete coverage.

Run `fastcontrol -usage-json` on a computer to inspect its aggregate reading. Keep that output private if it contains usage information you do not want to share.
