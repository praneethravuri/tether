# Security Policy

## Threat Model

- The trust boundary is a 0600 socket inside a 0700 directory.
- `SO_PEERCRED` is advisory logging only, not access control.
- Any process running as the same uid is trusted.
- `--as` is a name-collision guard, not authentication.

## Supported Versions

Tether is currently in active development. Security updates are provided for the latest release branch.

| Version | Supported          |
| ------- | ------------------ |
| 0.3.x   | :white_check_mark: |
| < 0.3.0 | :x:                |

## Reporting a Vulnerability

If you discover a security vulnerability in Tether, please report it immediately to help keep this project secure.

**Please do not report security vulnerabilities via public GitHub issues.**

Instead, please send an email to: **pravdevrav@gmail.com**

In your report, please include:
1. A description of the vulnerability.
2. Steps to reproduce the issue (including any proof-of-concept scripts or commands).
3. The potential impact of the vulnerability.

We will acknowledge receipt of your report within 48 hours and work with you to coordinate a security release to address the issue. Thank you for helping keep Tether secure!
