# Security policy

## Reporting a vulnerability

Email **security@github.com/zdaniels/fathom** with:

- A description of the vulnerability and the impact you believe it has.
- Steps to reproduce, ideally with a minimal proof-of-concept.
- Your name + handle if you'd like credit in the disclosure.

Please do **not** open a public GitHub issue — that's the canonical
way to get an attacker reading along while we're still patching.

## What to expect

| When | What |
|---|---|
| Within 48 hours | Acknowledgement that we received your report. |
| Within 7 days | Triage decision: confirmed / needs more info / not-a-vuln, with reasoning. |
| Within 30 days | Patch shipped for confirmed criticals + high-severity issues. |
| At fix time | Credit to you in the release notes + a public advisory if you want one. |

For lower-severity issues we may bundle the fix into the next minor
release rather than ship out-of-band — we'll tell you the plan in
the triage response.

## Supported versions

Only the **latest minor release** receives security patches. The
project moves quickly during pre-1.0; please run the most recent
version before reporting.

## Out of scope

- Vulnerabilities in third-party dependencies that aren't reachable
  from this package's actual code paths. Report those upstream; we
  bump versions when fixes land.
- Social-engineering / phishing of maintainers.
- Self-XSS or anything requiring an attacker who already has control
  of the victim's machine.

## Hall of fame

Researchers who've helped harden recall — listed with permission. Be
the first by reporting something! See above for the email.
