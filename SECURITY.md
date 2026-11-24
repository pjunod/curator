# Security policy — report vulnerabilities privately

For the automated checks and their limits, read
[Security checks](docs/SECURITY_CHECKS.md).

## Report a vulnerability

Use [GitHub private vulnerability reporting](https://github.com/pjunod/curator/security/advisories/new)
for suspected vulnerabilities. Do not put credentials, private library data,
or exploitable details in public issues or pull requests.

Include the affected version or commit, deployment configuration, impact,
and the smallest reproducible example. Remove tokens and personal data from
logs. Coordinate any public disclosure with the maintainer in the private
report. No response or fix deadline is promised.

## Supported versions

Security fixes are developed on `main`. Report issues against the latest
revision and identify any affected release or tag. Older versions do not
have a separate security-maintenance branch; upgrade to a fixed revision
when one is available.

## Deployment boundaries

Keep the database and backups on local storage, restrict access to the
configuration directory, and use HTTPS or a trusted VPN for remote access.
Follow the [deployment guide](docs/deployment.md) for storage and access
requirements. Automated checks do not establish that a deployment is safe.
