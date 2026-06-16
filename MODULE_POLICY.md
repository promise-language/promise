# Promise Lang Module Policy

This document defines what belongs in the official `promise-language` organization,
what belongs in the `promise-community` organization, and the legal reasoning
behind that separation.

---

## Overview

The Promise Lang ecosystem is divided into two GitHub organizations:

| Organization | Purpose | Governance | Legal Cover |
|---|---|---|---|
| [`promise-language`](https://github.com/promise-language) | Compiler, runtime, stdlib, official tooling | Core team, CLA required | Promise Lang LLC |
| [`promise-community`](https://github.com/promise-community) | Community modules, bindings, extensions | Community governed | Independent maintainers |

This separation is intentional. It allows the ecosystem to grow freely while
keeping the core project legally clean and professionally maintained.

---

## What Belongs in `promise-language`

A repository may be hosted under the `promise-language` organization if it meets
**all** of the following criteria:

1. **Original work** — The code is written from scratch or derived only from
   permissively licensed sources (MIT, BSD, Apache 2.0, ISC, or similar).

2. **No copyleft dependencies** — The module does not link against, embed, or
   distribute code under GPL, LGPL, AGPL, EUPL, or any other license that
   imposes conditions on downstream users or distributors.

3. **CLA signed** — All contributors have signed the Promise Lang Contributor
   License Agreement before their code is merged.

4. **Approved by core team** — The module has been reviewed and accepted by a
   core maintainer. Acceptance is not guaranteed and is based on scope,
   quality, and long-term maintenance commitment.

5. **Dual-license compatible** — The module is licensed under the same dual
   Apache License 2.0 / MIT License terms as the core project, or under a
   permissive license (MIT, BSD, ISC, or similar) that is compatible with both.
   Modules licensed under Apache 2.0 only are accepted but discouraged, as they
   fragment the ecosystem's dual-license convention.

---

## What Belongs in `promise-community`

A module should live in `promise-community` (or an entirely independent
organization) if any of the following apply:

- It wraps or links against a GPL, LGPL, or AGPL library
- It ports code from a copyleft project
- Its IP provenance is uncertain or disputed
- It is experimental, unstable, or not yet ready for core review
- It is maintained by a third party who does not wish to sign a CLA
- It is a binding to a proprietary or closed-source system

Community modules are **not** officially endorsed by Promise Lang LLC. They are
independently maintained and their authors bear full responsibility for
licensing, correctness, and maintenance.

---

## Legal Disclaimer for Community Modules

> The `promise-community` organization and any modules hosted there are
> independent community contributions. They are not reviewed, endorsed,
> warranted, or supported by Promise Lang LLC. Use of community modules is at
> your own risk. Promise Lang LLC makes no representations regarding the
> licensing, security, or fitness for purpose of community modules.

This disclaimer appears in the `promise-community` organization README and in
the README of each community module repository.

---

## Module Promotion

A community module may be promoted into `promise-language` if:

1. The maintainer signs the CLA
2. The module passes a full licensing audit (no copyleft contamination)
3. The core team agrees to take on long-term maintenance responsibility
4. The module meets code quality and API consistency standards

Promotion is not a right. It is a joint decision between the maintainer and
the core team.

---

## Linking to Community Modules

The official Promise Lang documentation and standard library index may link to
community modules for discoverability. Such a link is informational only and
does not constitute an endorsement, warranty, or assumption of legal
responsibility.

---

## Questions

If you are unsure whether your module belongs in `promise-language` or
`promise-community`, open a discussion in the main repository before submitting
a pull request or requesting org membership. The core team will advise.
