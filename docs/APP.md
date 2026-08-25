# The app — plan

> Status: plan. Nothing built. **The stack is deliberately not chosen yet** —
> see *The decision that comes first*.

Companion to [IDEA.md](IDEA.md) (what the product is) and
[BACKEND.md](BACKEND.md) (what already exists). The backend is done: it polls,
keeps history, holds watches, and notifies. The app is the last piece.

## What it is for

One thing, mostly: **arm a watch in two taps while standing outside in the
cold.** Everything else is secondary and can wait.

The moment this serves is specific. You are already outside. You have already
opened all three operator apps. There is nothing. You want to say "tell me"
once and put the phone back in your pocket. If arming a watch takes more than a
few seconds or more than two taps, the app has failed at the only job that
justifies its existence.

## Screens

**Now.** What is within reach, and how far the nearest of each operator is when
nothing is. This is `GET /api/v1/status` rendered honestly — a bare zero is a
bad answer, and "none here, but a Ryde 258 m away" is a walk rather than a
defeat. Pull to refresh; no polling in the background, because the backend is
already doing that.

**Arm.** One prominent control: *I need a scooter*. Defaults come from the last
watch armed, so the common case is a single tap. Behind it: radius, which
operators count, and how long to keep watching. Position comes from the phone
by default, with saved places as alternatives.

**Armed.** What is currently watching, how long it has left, and a way to cancel.
Short — usually empty or one row.

**History.** The count over the day and when vehicles arrived, from
`/api/v1/history` and `/api/v1/arrivals`. This is the screen that answers *what
time should I leave*, which may end up more valuable than the alerts. It is
also the least urgent to build.

## Constraints that shape it

**Notification delivery is not the app's job.** ntfy already does it, has a real
native app on both platforms, and the notification's tap action opens the
operator's own app on the exact vehicle. The app does not need push at all to
be useful, and should not grow its own notification stack just to own that hop.
This is the single biggest simplification available and it should survive
whatever else changes.

**Reachability is the real problem, not the UI.** The backend binds to loopback.
Reaching it from a phone means exposing it, and the vhost pattern used for
everything else here restricts to the home network — which is exactly the
network you are *not* on when standing on a street wanting a scooter. Options,
roughly in order of effort: reach it over the existing VPN, expose the API
behind its bearer token, or put it behind the same SSO as everything else and
accept that an interactive login every morning defeats the feature. The token
exists in the backend already for this reason.

**Authentication has to be invisible.** A long-lived per-device token entered
once. Anything that prompts in the morning has failed.

**It has to work with nothing to show.** The interesting state is the empty one.
A design that only looks right with six scooters in a list is the wrong design.

## The decision that comes first

**The stack is open.** It should be chosen against the constraints above rather
than by preference, and the honest inputs are:

- The app is small: four screens, one of which is a button, and a read-only
  HTTP API behind all of it. This is not a stack-defining problem.
- **It does not need push**, which removes the usual reason to go native.
- It does need **geolocation**, which every option provides.
- It will be used **outdoors, one-handed, in a hurry**, on one person's phone.
- Whatever is chosen has to be maintainable by whoever is standing in the cold
  when it breaks.

Candidates worth weighing, with the obvious tension stated rather than hidden:

| | Pull | Push back |
|---|---|---|
| **PWA** | No store, no build pipeline, instant deploy, one codebase | Home-screen install is a step; iOS has historically been where PWA edges are roughest |
| **Expo / React Native** | Real app, real geolocation, one codebase, easy path to native later | A build and distribution story for an audience of one |
| **Native** | Best integration | Hard to justify for four screens and no push |
| **No app at all** | A phone-friendly web page served by the daemon, plus ntfy for alerts. Smallest possible thing that satisfies the actual moment | Feels less like a product; no offline state |

That last row deserves a serious look rather than being dismissed. The backend
already serves HTTP and already has the summary sentence; ntfy already owns
notifications. A single responsive page may be the whole app.

## Sequence

1. **Settle reachability.** How the phone reaches the backend, and how it
   authenticates. Nothing can be built against an API that is not reachable
   from the street.
2. **Choose the stack**, against the constraints above.
3. **Build *Now* and *Arm*.** Those two are the product.
4. **Add *Armed*.**
5. **Add *History*** once there is enough data for it to say something.

## Open questions

- Is a saved place even needed, given the phone knows where it is? Possibly for
  "watch home while I am not there yet".
- Should the app show other operators when you only pay for one? Probably yes,
  quietly — a Voi at 40 m is worth knowing about even on a Ryde subscription.
- Does the *History* screen justify itself, or is it a chart nobody opens twice?
