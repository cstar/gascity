---
name: gc-skipper
description: Reach a rig's skipper — the per-rig interactive lead session — by mail, and use skippers as the entry point to coordinate across rig boundaries
---

# Reaching a rig's skipper

A **skipper** is a per-rig interactive lead: one live session pointed at a
single rig, the human-facing entry point for that rig's work. Because there
is one per rig, skippers are the natural way to **cross a rig boundary**.
When something needs checking or doing inside another rig's domain — its
platform, its deploys, its code — mail that rig's skipper instead of
guessing at pools or acting blind in a rig you don't own.

Run this when a problem or follow-up lands in a rig that is not yours.

## Find the skipper

Skipper sessions are named `skipper-<rig>` (e.g. `skipper-hatch`,
`skipper-voxist-api`). List the live ones:

```
gc session list | grep -i skipper
```

The addressable name is the exact session name (`skipper-<rig>`), **not** the
bare `skipper` alias — that alias is shared by every skipper and will not
route. If several match, take the one whose suffix is the rig you mean.

## Send

```
gc mail send skipper-<rig> -s "<subject>" -m "<body>" --notify
```

- `--notify` nudges the recipient even if earlier mail is unread — use it so
  a cross-rig ask surfaces promptly rather than waiting on their next poll.
- Add `--json` to confirm delivery: the result shows `"to":"skipper-<rig>"`
  and `"notified":true`.
- The sender defaults to your own session, so the recipient sees who it is
  from and can reply.

## Write it so they can act without you

A skipper on another rig has none of your context. Put in the body:

- the concrete artifact — app slug, PR, URL, bead id — not "the deploy"
- the symptom **and** the evidence you already gathered (status, logs, what
  you ruled out), so they verify rather than re-investigate from zero
- the specific question or action you want from them

A vague ping ("can you look at hatch?") just makes them ask you what you
already know.

## When to use

- A problem sits in another rig's domain (platform, infra, a sibling
  service) and its skipper can verify or act faster than you can from
  outside.
- You are handing a cross-rig follow-up to the lead who owns that rig.
- You want a second pair of eyes from the rig that owns the system.

Reading and replying are covered by the **gc-mail** skill; this skill is
about *who* to reach across rigs, and *why* the skipper is that entry point.
