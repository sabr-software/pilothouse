# Several machines in one place: `pilothouse link`

Run agents on more than one computer — a desktop, a laptop, a box under the
desk — and see all of them in one app. One machine is the **hub**: your phone
is paired with it, and it shows its own sessions plus those of every machine
linked to it. Answers, replies and the live terminal reach the machine that
runs the agent.

## Set up

Every machine needs Pilothouse running and remote access on, so the machines
can reach each other over your tailnet:

```sh
pilothouse remote tailscale          # on every machine
```

On the hub (the machine your phone is paired with):

```sh
pilothouse link add
```

It prints a command with a one-time code (10 minutes). Run it on the other
machine:

```sh
pilothouse link join https://studio.tailnet.ts.net 4KQ2-9XHT-M3PB
```

The hub first checks it can reach that machine. Then the machine shows the
hub's paired devices with a fingerprint of each key and asks whether to trust
them. Compare each with the one the hub shows (`pilothouse devices` there, or
Settings → This device in the app): only the keys you were shown are trusted,
so a key that changes in between stays untrusted. Trusted devices can answer
this machine's requests, reply to its agents and type into their terminals.
Repeat for each machine. Every machine needs its own name (`machine:` in
config.yaml); joining with a name already in use is refused.

```sh
pilothouse link            # or `link ls`: the hub, or the linked machines, and whether they're online
pilothouse link trust      # on a linked machine: trust the hub's new devices
pilothouse link remove laptop   # on the hub: unlink a machine; on a linked machine: leave the hub
```

## In the app

- **Today** shows every machine's sessions together. Sessions on another
  machine carry its name, and chips at the top narrow Today to one machine
  (the choice is remembered on that device). An offline machine is marked,
  and its sessions are shown as last seen.
- A session's **report** says which machine it runs on.
- **Settings → Machines** lists the machines and whether they're online.
- Setting **Away** or **At my desk** from the app applies to every machine.
- Notifications from linked machines arrive through the hub's channels (Web
  Push, Telegram, ntfy, Slack, Discord), with the machine's name in the title.

Rules, tasks and insights stay per machine: the app shows the hub's. **Make it
a rule** isn't offered for a linked machine's request; add the rule on that
machine (`pilothouse policy add …`).

## How it works

- The hub follows each linked machine's event stream over the machine's web
  URL and merges it with its own into one stream with sequence numbers of its
  own, so the app works as before. When a machine reconnects, the hub replays
  its current sessions and requests into the stream.
- A device call about a linked machine's session or request is forwarded to
  that machine unchanged, naming the device. The linked machine runs the same
  handler a paired device would reach, so **a signed answer is checked on the
  machine that runs the agent**, against the device's key.
- Linking exchanges two random 256-bit tokens; each side stores the token it
  sends and only a hash of the one it receives. A token lets the hub read
  sessions and forward calls; **it can't answer anything by itself**: answers,
  replies, terminal input and the rest need a device signature.
- A linked machine trusts a device only for the key a person confirmed there
  (`link join` or `link trust`). A device that pairs with the hub later, or
  gets a new key (paired again, restored with a passkey), can see the
  machine's sessions but not act until you run `pilothouse link trust` on that
  machine. A device revoked on the hub is revoked on every linked machine.
- Only the hub's devices are involved: linked machines send their
  notifications to the hub, which delivers them without one-tap Telegram or
  ntfy buttons (those would need a signature the hub can't make). Answer in
  the app.
- One hub per machine, and a hub can't also be linked to another hub.
- Linking, trusting and unlinking are refused from inside an agent's process
  tree, like pairing.

## Limits

- Machines must reach each other: on a tailnet with HTTPS (`pilothouse remote
  tailscale`), or on one computer over `http://127.0.0.1`. Machines on
  different networks without Tailscale aren't supported yet.
- The terminal inbox (`pilothouse`) and the CLI show this machine only.
- Rules, tasks, insights and budgets are per machine.
