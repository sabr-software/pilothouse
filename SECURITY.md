# Security policy

Pilothouse decides whether coding agents may run commands, so security reports
are the most important issues we get.

## Reporting a vulnerability

Please report privately through GitHub's
[private vulnerability reporting](https://github.com/sabr-software/pilothouse/security/advisories/new).
Don't open a public issue. You'll get an acknowledgement within a few days, and
credit in the release notes if you'd like it.

Useful details: the version (`pilothouse version`), OS, how the daemon is
reachable (local only, Tailscale Serve), and the smallest steps that reproduce it.

## Supported versions

Only the latest release receives fixes while Pilothouse is 0.x.

## Scope and known limits

The threat model and its honest limits are in the
[security model](https://pilothouse.dev/docs/security). In short: remote
approvals are signed per device and replay-protected; the agent-caller check
and the self-protection rule are defense in depth, not a sandbox. A process
running as your user that double-forks away from the agent escapes the
ancestry check, and an obfuscated command can escape the rule. Reports that
improve either are welcome.
