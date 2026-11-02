package policy

// StarterPolicy is written by `pilothouse policy init`. Every rule in it is
// a working example; tests compile it and check its behaviour.
const StarterPolicy = `# Pilothouse policy — applied before every tool call of every agent.
# Reference: https://github.com/sabr-software/pilothouse/blob/main/docs/POLICY.md
#
#   action: deny   block the call; the agent sees your message
#   action: ask    make a person confirm (the agent's own prompt at the desk,
#                  your phone when you're away)
#   action: allow  skip the agent's own permission prompt
#
# Precedence: if any matching rule says deny, the call is denied; else if any
# says ask, a person is asked; else allow. Order in this file only breaks ties.
# Built-in rule pilothouse-self-protection always asks before an agent runs
# pilothouse admin commands or edits Pilothouse/agent settings.
#
# match:  globs, all fields AND-ed. Fields: agent, tool, kind, command, path,
#         cwd, repo, branch. kind is one of shell, edit, read, web, mcp, other.
#         * ** ? {a,b}; a list means "any of". command globs see the whole
#         command line; path matches if any file the tool touches matches;
#         relative path globs match at any depth (".env" = "**/.env").
# when:   optional CEL expression, AND-ed with match. Variables: agent, tool,
#         kind, command, paths, path, input, cwd, repo, branch, mode, hour,
#         weekday. Extras: glob(s, pattern), has_word(s, word), strings ext.
#
# Check your edits with:  pilothouse policy check
# Try a call with:        pilothouse policy test --command "git push -f origin main"
version: 1
rules:
  # --- Destructive commands -------------------------------------------------

  - name: no-force-push
    description: Never rewrite shared history from an agent
    match: { kind: shell, command: "*git*push*" }
    when: 'has_word(command, "--force") || has_word(command, "-f") || command.matches(r"\spush\s[^;&|]*\s\+[^\s]")'
    action: deny
    message: Force-push is blocked by policy. Push to a new branch and open a PR instead.

  - name: no-rm-rf-root
    description: Refuse recursive deletes of /, ~ or $HOME
    match: { kind: shell, command: "*rm *" }
    when: >-
      command.matches(r'\brm\s[^;&|]*(-[a-zA-Z]*[rR]|--recursive)')
      && command.matches(r'\brm\s[^;&|]*\s(/|/\*|~|~/|~/\*|\$HOME|\$HOME/|\$HOME/\*|\.\.|\.\./)(\s|;|&|\||$)')
    action: deny
    message: Recursive delete of a root or home directory is blocked by policy.

  # --- Secrets -------------------------------------------------------------

  - name: no-secret-files
    description: Keep agents out of credentials and .env files (templates like .env.example are fine)
    match:
      path: [".env", ".env.*", "*.pem", "*.key", "id_rsa*", "id_ed25519*", "~/.ssh/**", "~/.aws/credentials", "~/.config/gh/hosts.yml"]
    when: '!paths.all(p, p.endsWith(".example") || p.endsWith(".sample") || p.endsWith(".template") || p.endsWith(".pub"))'
    action: deny
    message: This file holds secrets; Pilothouse policy keeps agents out of it. Ask the user to provide what you need.

  - name: ask-secret-files-in-shell
    description: Shell commands that touch .env or SSH/AWS credentials need a person
    match: { kind: shell }
    when: >-
      command.matches(r'(^|[\s"=/])\.env(\.(local|dev|development|prod|production|staging|test))?($|[\s";|&)])')
      || command.matches(r'\.ssh/id_|\.aws/credentials')
    action: ask

  # --- Things a person should see ------------------------------------------

  - name: ask-push-to-main
    description: Pushing to main/master goes through a person
    match: { kind: shell, command: "*git*push*" }
    when: >-
      has_word(command, "main") || has_word(command, "master")
      || (branch in ["main", "master"]
          && (!command.matches(r'git\s+push\s+[^-\s]\S*\s+[^-\s]') || has_word(command, "HEAD")))
    action: ask
    message: Pushing to the main branch needs a person's OK.

  - name: ask-pipe-to-shell
    description: Running a downloaded script (curl … | sh) needs a person
    match: { kind: shell }
    when: 'command.matches(r"(curl|wget)\s[^|]*\|\s*(sudo\s+)?(ba|z|da)?sh\b")'
    action: ask
    message: Piping a download into a shell needs a person's OK.

  # --- Fewer prompts for safe things ---------------------------------------

  - name: allow-read-only
    description: Reading and searching files never needs a prompt (deny rules above still win)
    match: { kind: read }
    action: allow

  # --- More ideas (uncomment to use) ---------------------------------------
  #
  # - name: codex-no-network
  #   agents: [codex]
  #   match: { kind: web }
  #   action: deny
  #
  # - name: ask-after-hours
  #   description: Outside working hours, every edit outside ~/scratch needs a person
  #   match: { kind: edit }
  #   when: '(hour < 8 || hour >= 20 || weekday == 0 || weekday == 6) && !paths.all(p, glob(p, "~/scratch/**"))'
  #   action: ask
  #
  # - name: allow-tests-in-work-repos
  #   match: { kind: shell, repo: "~/work/**", command: ["go test*", "npm test*", "pytest*"] }
  #   action: allow
  #
  # - name: ask-deploys
  #   match: { kind: shell, command: ["*terraform apply*", "*kubectl apply*", "*fly deploy*"] }
  #   action: ask
`
