# Security policy

Bayplan manages Docker hosts, and access to the Docker API is root-equivalent, so we treat security reports seriously. [docs/threat-model.md](docs/threat-model.md) explains what Bayplan protects and the risks that are already known.

## Supported versions

Only the latest 0.x minor release receives security fixes. Bayplan is pre-1.0 and breaking changes are allowed between minors, so a fix may require moving to the newest minor. Images tagged `:edge` follow the `main` branch and are not a supported release.

## Reporting a vulnerability

**Report privately. Do not open a public issue, pull request or discussion for a vulnerability.**

Use GitHub Security Advisories: on the repository at <https://github.com/ianglennon/bayplan>, open the **Security** tab and choose **Report a vulnerability**. This creates a private report that only you and the maintainers can see, and lets us fix and publish an advisory together.

Please include:

- the Bayplan version or image tag and commit (**Settings > About** shows both);
- the Docker Engine version and how Bayplan was installed;
- what you found, and the impact you think it has;
- steps to reproduce, or a proof of concept, and any logs that help (remove secrets first: the setup token, `bayplan.key` and session cookies must not be in a report);
- whether you intend to publish the finding, and by when.

## What to expect

We aim to reply within a few working days, to confirm or dispute the report within about two weeks, and to agree a disclosure date with you once a fix exists. These are goals for a volunteer-run project, not guarantees. We will credit you in the advisory unless you prefer otherwise.

## Scope

In scope: the Bayplan server and web interface, the container image, the build and release pipeline, and the documentation where it gives unsafe advice.

Out of scope: weaknesses that need prior root access to the host or the Docker socket (that access is already full control, see the threat model), denial of service by an admin, and the risks listed under "Residual risks" in the threat model unless you have found a way to make them worse than described there.
