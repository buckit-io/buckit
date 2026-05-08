# Security Policy

## Supported Versions

We always provide security updates for the [latest release](https://github.com/buckit-io/buckit/releases/latest).
Whenever there is a security update you just need to upgrade to the latest version.

## Reporting a Vulnerability

All security bugs in [buckit-io/buckit](https://github.com/buckit-io/buckit)
should be reported by opening a private issue on GitHub or by contacting the maintainers.

Please, provide a detailed explanation of the issue. In particular, outline the type of the security
issue (DoS, authentication bypass, information disclose, ...) and the assumptions you're making (e.g. do
you need access credentials for a successful exploit).

### Disclosure Process

Buckit uses the following disclosure process:

1. Once the security report is received one member of the security team tries to verify and reproduce
   the issue and determines the impact it has.
2. A member of the security team will respond and either confirm or reject the security report.
   If the report is rejected the response explains why.
3. Code audit is performed to find any potential similar problems.
4. Fixes are prepared for the latest release.
5. On the date that the fixes are applied a security advisory will be published.
   Please inform us in your report whether Buckit should mention your contribution w.r.t. fixing
   the security issue. By default Buckit will **not** publish this information to protect your privacy.

This process can take some time, especially when coordination is required with maintainers of other projects.
Every effort will be made to handle the bug in as timely a manner as possible, however it's important that we
follow the process described above to ensure that disclosures are handled consistently.
