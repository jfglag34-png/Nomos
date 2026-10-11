# Governance

WEDRA is a young, currently single-maintainer project. This document makes
that limitation explicit instead of presenting a fictional maintainer quorum.
The repository owner currently holds merge and release authority.

## How changes are decided

- Bug fixes and small documentation changes use public issues and pull requests.
- Protocol changes, registry policy changes, breaking CLI changes, and repository
  reorganizations require a public proposal and an ADR or migration note before
  implementation.
- A proposal should state compatibility impact, migration steps, tests, and the
  version axis it changes.
- Released tags are immutable. A correction after publication uses a new
  release version (standard SemVer or an explicitly approved letter-suffix);
  it never rewrites or reuses an existing tag.

## Releases

A release is publishable only when:

1. `VERSION`, the current `CHANGELOG.md` entry, and the release tag agree.
2. CI, conformance tests, and registry validation are green.
3. The release notes identify product, protocol, registry, and plugin changes
   separately.

The release checklist is intentionally small enough for a volunteer maintainer
to repeat without private knowledge.

## Plugins outside the core

There are no third-party plugins today. Every plugin in `registry.yaml` is
written by the repository owner, and every entry's `source` is this repository.
Admission is currently the owner's decision, not an independent review, and the
`author` field in the manifests is a placeholder rather than a named person.

Admission requires:

- a truthful manifest and permissions declaration;
- conformance tests and a reviewable description;
- no embedded credentials or undisclosed network access.

The registry is the publication boundary. A plugin outside `registry.yaml` is
an in-tree experiment, not a supported marketplace entry.

## Reducing maintainer risk

New ownership is encouraged from sustained contributors. The project records
decisions in public, keeps compatibility surfaces explicit, and avoids
unilateral mass renames. Anyone interested in co-maintaining a package,
protocol area, or release process should open a public issue describing the
scope they want to own.

This is a risk-reduction process, not a guarantee of response time or staffing.
