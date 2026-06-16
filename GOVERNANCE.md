# Governance

This document describes how decisions are made on the Promise project today. It
is intentionally lightweight and will evolve as the project and its contributor
base grow.

## Current model

Promise is an early-stage, founder-led project. **Its founder is the lead
maintainer and final decision-maker** for the language, compiler, standard
library, and catalog. The project is stewarded by **Promise Lang LLC** (Texas),
which holds the project's IP and trademarks; see [`MODULE_POLICY.md`](MODULE_POLICY.md)
for how the official `promise-language` organization relates to the independent
`promise-community` organization.

This is the honest state of things for a young project: most decisions are made
quickly by the lead maintainer, in the open, on issues and pull requests.

## Making changes

- **Routine changes** — bug fixes, tests, docs, and small, self-contained
  improvements — go through a normal pull request and review. Contributions
  require a signed Contributor License Agreement; see
  [`CONTRIBUTING.md`](CONTRIBUTING.md), [`INDIVIDUAL_CLA.md`](INDIVIDUAL_CLA.md),
  and [`CORPORATE_CLA.md`](CORPORATE_CLA.md).

- **Significant changes** — anything that affects language semantics, the
  standard library surface, the catalog/epoch model, or the toolchain's public
  behavior — start with a **lightweight written proposal (RFC-lite)** before a
  large implementation lands. Open an issue or discussion describing the problem,
  the proposed design, and the alternatives considered. Accepted proposals are
  captured as a **design-decision document** so the reasoning is recorded
  alongside the code. The goal is a clear written record, not heavy process.

## Decisions and disputes

While the project is young, the **lead maintainer decides**, and records the
rationale for consequential decisions in the relevant proposal or design-decision
doc. Disagreement is welcome and best raised in the open on the issue or PR; the
maintainer's decision is final for now.

## Toward a core team

As regular, trusted contributors emerge, the project expects to grow into a
**small core team** of maintainers with merge rights and a shared say in
significant decisions. Membership will be earned through a sustained track record
of high-quality contributions and good judgment. This document will be updated to
describe that team — and a more formal decision process — once it actually
exists. We are not inventing committees, voting rules, or bylaws ahead of need.

## Code of conduct

Everyone participating in the project is expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).
