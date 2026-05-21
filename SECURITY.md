# Security Policy

## Supported versions

Only the latest commit on `main` is actively maintained. No LTS or patch branches exist at this stage.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Email `security@lgreene03.dev` (or use GitHub's private security-advisory feature: **Security → Report a vulnerability**) with:

- A description of the vulnerability and the affected component.
- Steps to reproduce or a minimal proof-of-concept.
- Your assessment of impact and severity.

You will receive an acknowledgement within 48 hours. We aim to ship a fix within 14 days for critical issues and 30 days for moderate/low issues.

## Scope

The following are **in scope**:

- Remote code execution via Huginn's HTTP API (`/api/*`)
- Authentication bypass on token-gated endpoints
- SQL injection via the PostgreSQL journal path
- Sensitive data (credentials, trading parameters) exposed in logs or API responses

The following are **out of scope**:

- Vulnerabilities in third-party dependencies (report those upstream)
- Issues requiring physical access to the host
- Theoretical attacks with no realistic threat model for a paper-trading engine

## Security model

Huginn is designed for **operator-trusted deployments**. It is not a multi-tenant SaaS system. The bearer-token auth on mutating endpoints (`HUGINN_API_TOKEN`) is a minimal safeguard for network-exposed deployments; it is not intended to replace VPN/firewall controls in a production environment.
