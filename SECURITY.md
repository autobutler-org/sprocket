# Security Policy

Sprocket parses video files. Those files often come from somewhere untrusted, and a parser bug in a library is a bug in
every program that imports it. We take reports seriously.

## Supported Versions

Only the latest tagged release is supported. Fixes ship in the next tag rather than as backports — there are no
maintained release branches. Releases are listed on the
[releases page](https://github.com/autobutler-org/sprocket/releases).

## Reporting a Vulnerability

**Do not open a public issue.** File a private security advisory:

<https://github.com/autobutler-org/sprocket/security/advisories/new>

Please include:

- What you found, and which package it affects
- Steps to reproduce, with the input file if one is needed
- The version you tested
- What an attacker could actually do with it, and what access they'd need first

We'll acknowledge the report, tell you whether we consider it a vulnerability, and let you know when a fix is out. If
you'd like credit in the advisory, say so and tell us how you want to be named.

Please give us a chance to ship a fix before publishing.

## In scope

- Memory-unsafe or panicking behavior reachable from a crafted input file
- Unbounded allocation, unbounded read, or a hang on a crafted input file — a decompression bomb or a malformed box
  length that makes a caller consume the machine
- Reading or writing paths outside those the caller asked for
- Anything that sends data anywhere. Sprocket makes no network calls; one appearing is a vulnerability by itself
- The dependency supply chain and the release pipeline

## Out of scope

- Crashes on input the caller explicitly marked as trusted
- Resource use that is proportional to the input you handed it — a 40GB file legitimately takes a while
- Scanner output with no demonstrated impact
- Vulnerabilities in a program that imports Sprocket but not in Sprocket itself

If you're unsure whether something is in scope, file the advisory anyway and let us decide.

## Non-critical concerns

Hardening ideas, defense-in-depth improvements, and non-exploitable concerns can go in the public tracker using the
[security issue template](https://github.com/autobutler-org/sprocket/issues/new?template=security.yaml).
